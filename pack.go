// Packfile codec: pkt-line framing, delta-free pack writing (zlib
// BestSpeed), pack reading with OFS/REF delta resolution.
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ---------------------------------------------------------------- pkt-lines

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 4, len(payload)+4)
	const hexd = "0123456789abcdef"
	out[0] = hexd[(n>>12)&0xf]
	out[1] = hexd[(n>>8)&0xf]
	out[2] = hexd[(n>>4)&0xf]
	out[3] = hexd[n&0xf]
	return append(out, payload...)
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

type pktItem struct {
	kind    string // "flush" | "delim" | "data"
	payload []byte
}

func decodePkts(buf []byte) []pktItem {
	var out []pktItem
	off := 0
	for off+4 <= len(buf) {
		h := string(buf[off : off+4])
		if !isHex4(h) {
			break
		}
		n := hexVal(h)
		if n == 0 {
			out = append(out, pktItem{kind: "flush"})
			off += 4
			continue
		}
		if n == 1 {
			out = append(out, pktItem{kind: "delim"})
			off += 4
			continue
		}
		if n == 2 {
			out = append(out, pktItem{kind: "response-end"})
			off += 4
			continue
		}
		if n < 4 || off+n > len(buf) {
			break
		}
		out = append(out, pktItem{kind: "data", payload: buf[off+4 : off+n]})
		off += n
	}
	return out
}

func isHex4(s string) bool {
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func hexVal(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		default:
			v = int(c-'A') + 10
		}
		n = n*16 + v
	}
	return n
}

// ---------------------------------------------------------------- pack writing

var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

func deflateBestSpeed(data []byte) []byte {
	var buf bytes.Buffer
	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(&buf)
	w.Write(data)
	w.Close()
	zlibWriterPool.Put(w)
	return buf.Bytes()
}

func packObjHeader(t objType, size int) []byte {
	code := byte(t)
	out := []byte{}
	first := (code << 4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, first)
	return out
}

// writePack: delta-free pack, objects in the given order.
func writePack(objs []*gitObj) []byte {
	var body bytes.Buffer
	body.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	body.Write(hdr[:])
	for _, o := range objs {
		body.Write(packObjHeader(o.typ, len(o.data)))
		body.Write(deflateBestSpeed(o.data))
	}
	sum := sha1hex(body.Bytes())
	trailer, _ := hex.DecodeString(sum)
	return append(body.Bytes(), trailer...)
}

// ---------------------------------------------------------------- pack reading

// countingReader implements flate's reader interface (Read + ReadByte)
// over a bytes.Reader and counts consumed bytes exactly: zlib's
// decompressor reads only what it needs, so after a successful inflate
// the count is the exact compressed length (verified empirically).
type countingReader struct {
	r *bytes.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func inflateAt(pack []byte, off, limit int) ([]byte, int, error) {
	cr := &countingReader{r: bytes.NewReader(pack[off:limit])}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	return data, off + cr.n, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		v := 0
		sh := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size truncated")
			}
			b := delta[off]
			off++
			v += int(b&0x7f) << sh
			sh += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, tgtSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			co, cs := 0, 0
			sh := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset truncated")
					}
					co += int(delta[off]) << sh
					off++
				}
				sh += 8
			}
			sh = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size truncated")
					}
					cs += int(delta[off]) << sh
					off++
				}
				sh += 8
			}
			if cs == 0 {
				cs = 0x10000
			}
			if co+cs > len(base) || written+cs > tgtSize {
				return nil, errors.New("delta copy out of range")
			}
			copy(out[written:written+cs], base[co:co+cs])
			written += cs
		} else if op != 0 {
			n := int(op & 0x7f)
			if off+n > len(delta) || written+n > tgtSize {
				return nil, errors.New("delta insert truncated")
			}
			copy(out[written:written+n], delta[off:off+n])
			off += n
			written += n
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if written != tgtSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type rawPackEntry struct {
	offset   int
	typeCode int
	size     int
	data     []byte
	baseOff  int
	baseOID  string
}

var errPack = errors.New("bad pack")

func readPack(pack []byte) (map[string]*gitObj, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errPack
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return nil, errPack
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	raw := make([]*rawPackEntry, 0, count)
	byOff := make(map[int]*rawPackEntry)
	off := 12
	for i := 0; i < count; i++ {
		if off >= trailerStart {
			return nil, errPack
		}
		entryOff := off
		b := pack[off]
		first := b
		size := int(b & 0x0f)
		sh := uint(4)
		off++
		for b&0x80 != 0 {
			if off >= trailerStart {
				return nil, errPack
			}
			b = pack[off]
			off++
			size += int(b&0x7f) << sh
			sh += 7
		}
		typeCode := int((first >> 4) & 0x07)
		entry := &rawPackEntry{offset: entryOff, typeCode: typeCode, size: size}
		switch typeCode {
		case 6:
			if off >= trailerStart {
				return nil, errPack
			}
			by := pack[off]
			off++
			v := int(by & 0x7f)
			for by&0x80 != 0 {
				if off >= trailerStart {
					return nil, errPack
				}
				by = pack[off]
				off++
				v = ((v + 1) << 7) + int(by&0x7f)
			}
			entry.baseOff = entryOff - v
		case 7:
			if off+20 > trailerStart {
				return nil, errPack
			}
			entry.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		case 1, 2, 3, 4:
		default:
			return nil, errPack
		}
		data, next, err := inflateAt(pack, off, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack size mismatch")
		}
		entry.data = data
		off = next
		raw = append(raw, entry)
		byOff[entryOff] = entry
	}
	objects := make(map[string]*gitObj)
	resolved := make(map[int]*gitObj)
	byOID := make(map[string]*gitObj)
	var resolve func(e *rawPackEntry) (*gitObj, error)
	resolve = func(e *rawPackEntry) (*gitObj, error) {
		if hit, ok := resolved[e.offset]; ok {
			return hit, nil
		}
		var typ objType
		var data []byte
		switch e.typeCode {
		case 6:
			base, err := resolve(byOff[e.baseOff])
			if err != nil {
				return nil, err
			}
			typ = base.typ
			data, err = applyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
		case 7:
			inPack, ok := byOID[e.baseOID]
			if !ok {
				return nil, errors.New("ref-delta base missing")
			}
			typ = inPack.typ
			var err error
			data, err = applyDelta(inPack.data, e.data)
			if err != nil {
				return nil, err
			}
		default:
			typ = objType(e.typeCode)
			data = e.data
		}
		oid := hashObj(typ, data)
		obj := &gitObj{typ: typ, data: data}
		resolved[e.offset] = obj
		byOID[oid] = obj
		return obj, nil
	}
	for _, e := range raw {
		obj, err := resolve(e)
		if err != nil {
			return nil, err
		}
		oid := hashObj(obj.typ, obj.data)
		if _, ok := objects[oid]; !ok {
			objects[oid] = obj
		}
	}
	return objects, nil
}
