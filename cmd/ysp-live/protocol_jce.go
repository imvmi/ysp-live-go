package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

const (
	yspVersionName = "3.2.7.26212"
	yspVersionCode = 302070
	yspAppID       = "1200013"
	yspQMFAppID    = 10012
	yspQMFPlatform = 1
	yspBizID       = 0
	yspChannelID   = "10070"
	pidTimeShift   = 25312
)

type jceWriter struct {
	b []byte
}

func (w *jceWriter) head(typ, tag byte) {
	if tag < 15 {
		w.b = append(w.b, (tag<<4)|(typ&0x0f))
		return
	}
	w.b = append(w.b, 0xf0|(typ&0x0f), tag)
}

func (w *jceWriter) byte(v int64, tag byte) {
	if v == 0 {
		w.head(12, tag)
		return
	}
	w.head(0, tag)
	w.b = append(w.b, byte(int8(v)))
}

func (w *jceWriter) short(v int64, tag byte) {
	if v >= math.MinInt8 && v <= math.MaxInt8 {
		w.byte(v, tag)
		return
	}
	w.head(1, tag)
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(int16(v)))
}

func (w *jceWriter) integer(v int64, tag byte) {
	if v >= math.MinInt16 && v <= math.MaxInt16 {
		w.short(v, tag)
		return
	}
	w.head(2, tag)
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(int32(v)))
}

func (w *jceWriter) long(v int64, tag byte) {
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		w.integer(v, tag)
		return
	}
	w.head(3, tag)
	w.b = binary.BigEndian.AppendUint64(w.b, uint64(v))
}

func (w *jceWriter) float(v float32, tag byte) {
	w.head(4, tag)
	w.b = binary.BigEndian.AppendUint32(w.b, math.Float32bits(v))
}

func (w *jceWriter) double(v float64, tag byte) {
	w.head(5, tag)
	w.b = binary.BigEndian.AppendUint64(w.b, math.Float64bits(v))
}

func (w *jceWriter) str(s string, tag byte) {
	d := []byte(s)
	if len(d) > 255 {
		w.head(7, tag)
		w.b = binary.BigEndian.AppendUint32(w.b, uint32(len(d)))
	} else {
		w.head(6, tag)
		w.b = append(w.b, byte(len(d)))
	}
	w.b = append(w.b, d...)
}

func (w *jceWriter) bytes(d []byte, tag byte) {
	w.head(13, tag)
	w.head(0, 0)
	w.integer(int64(len(d)), 0)
	w.b = append(w.b, d...)
}

func (w *jceWriter) structure(tag byte, fn func(*jceWriter)) {
	w.head(10, tag)
	fn(w)
	w.head(11, 0)
}

func (w *jceWriter) emptyList(tag byte) {
	w.head(9, tag)
	w.integer(0, 0)
}

type jceReader struct {
	d []byte
	p int
}

func (r *jceReader) get(n int) ([]byte, error) {
	if n < 0 || r.p+n > len(r.d) {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.d[r.p : r.p+n]
	r.p += n
	return b, nil
}

func (r *jceReader) u8() (byte, error) {
	b, err := r.get(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *jceReader) head() (typ, tag byte, err error) {
	b, err := r.u8()
	if err != nil {
		return 0, 0, err
	}
	typ, tag = b&0x0f, b>>4
	if tag == 15 {
		tag, err = r.u8()
	}
	return typ, tag, err
}

func (r *jceReader) intValue() (int64, error) {
	typ, _, err := r.head()
	if err != nil {
		return 0, err
	}
	v, err := r.value(typ)
	if err != nil {
		return 0, err
	}
	switch n := v.(type) {
	case int64:
		return n, nil
	default:
		return 0, fmt.Errorf("JCE value is %T, not an integer", v)
	}
}

func (r *jceReader) fieldValue() (any, error) {
	typ, _, err := r.head()
	if err != nil {
		return nil, err
	}
	return r.value(typ)
}

func (r *jceReader) value(typ byte) (any, error) {
	switch typ {
	case 0:
		b, err := r.u8()
		return int64(int8(b)), err
	case 1:
		b, err := r.get(2)
		if err != nil {
			return nil, err
		}
		return int64(int16(binary.BigEndian.Uint16(b))), nil
	case 2:
		b, err := r.get(4)
		if err != nil {
			return nil, err
		}
		return int64(int32(binary.BigEndian.Uint32(b))), nil
	case 3:
		b, err := r.get(8)
		if err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint64(b)), nil
	case 4:
		b, err := r.get(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), nil
	case 5:
		b, err := r.get(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case 6:
		n, err := r.u8()
		if err != nil {
			return nil, err
		}
		b, err := r.get(int(n))
		return string(b), err
	case 7:
		b, err := r.get(4)
		if err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint32(b))
		b, err = r.get(n)
		return string(b), err
	case 8:
		n, err := r.intValue()
		if err != nil || n < 0 || n > 1_000_000 {
			return nil, fmt.Errorf("invalid JCE map length %d: %w", n, err)
		}
		m := make(map[any]any, n)
		for range n {
			k, err := r.fieldValue()
			if err != nil {
				return nil, err
			}
			v, err := r.fieldValue()
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, nil
	case 9:
		n, err := r.intValue()
		if err != nil || n < 0 || n > 1_000_000 {
			return nil, fmt.Errorf("invalid JCE list length %d: %w", n, err)
		}
		items := make([]any, 0, n)
		for range n {
			v, err := r.fieldValue()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return items, nil
	case 10:
		return r.structure()
	case 11:
		return nil, nil
	case 12:
		return int64(0), nil
	case 13:
		if _, _, err := r.head(); err != nil {
			return nil, err
		}
		n, err := r.intValue()
		if err != nil || n < 0 || n > int64(len(r.d)-r.p) {
			return nil, fmt.Errorf("invalid JCE byte length %d: %w", n, err)
		}
		return r.get(int(n))
	default:
		return nil, fmt.Errorf("unsupported JCE type %d", typ)
	}
}

func (r *jceReader) structure() (map[byte]any, error) {
	m := make(map[byte]any)
	for r.p < len(r.d) {
		typ, tag, err := r.head()
		if err != nil {
			return nil, err
		}
		if typ == 11 {
			break
		}
		v, err := r.value(typ)
		if err != nil {
			return nil, err
		}
		m[tag] = v
	}
	return m, nil
}

func newGUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("generate GUID: %v", err))
	}
	return hex.EncodeToString(b)
}

func writeQUA(w *jceWriter, guid string) {
	w.str(yspVersionName, 0)
	w.str(fmt.Sprint(yspVersionCode), 1)
	w.integer(1080, 2)
	w.integer(2400, 3)
	w.integer(3, 4)
	w.str("12", 5)
	w.integer(1, 6)
	w.integer(1, 7)
	w.integer(420, 8)
	w.str(yspChannelID, 9)
	for i := byte(10); i < 15; i++ {
		w.str("", i)
	}
	w.structure(15, func(ww *jceWriter) {
		ww.integer(0, 0)
		ww.byte(0, 1)
		ww.str("", 2)
	})
	w.str("", 16)
	w.str("", 17)
	w.str("", 18)
	w.structure(19, func(ww *jceWriter) {
		ww.integer(0, 0)
		ww.float(0, 1)
		ww.float(0, 2)
		ww.double(0, 3)
	})
	w.str(guid[:16], 20)
	w.str("Pixel 6", 21)
	w.integer(1, 22)
	for i := byte(23); i < 27; i++ {
		w.integer(0, i)
	}
	w.str("", 27)
	w.str("", 28)
	w.str(guid, 29)
}

func writeRequestHead(w *jceWriter, cmd int64, reqID int64, guid string) {
	w.integer(reqID, 0)
	w.integer(cmd, 1)
	w.structure(2, func(ww *jceWriter) { writeQUA(ww, guid) })
	w.str(yspAppID, 3)
	w.str(guid, 4)
	w.emptyList(5)
	w.structure(6, func(*jceWriter) {})
	w.emptyList(7)
	w.integer(0, 8)
	w.integer(0, 9)
	w.integer(0, 10)
}

func wrapJCE(cmd int64, body []byte, reqID int64, guid string) ([]byte, error) {
	w := &jceWriter{}
	w.structure(0, func(ww *jceWriter) { writeRequestHead(ww, cmd, reqID, guid) })
	w.bytes(body, 1)
	reqCmd := w.b

	inner := []byte{38}
	inner = binary.BigEndian.AppendUint32(inner, uint32(len(reqCmd)+17))
	inner = append(inner, 1)
	inner = append(inner, make([]byte, 10)...)
	inner = append(inner, reqCmd...)
	inner = append(inner, 40)

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(inner); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	out := []byte{19, 0, 0, 0, 0}
	out = binary.BigEndian.AppendUint16(out, 2)
	out = binary.BigEndian.AppendUint16(out, 65281)
	out = binary.BigEndian.AppendUint16(out, uint16(cmd))
	out = binary.BigEndian.AppendUint16(out, 0)
	out = binary.BigEndian.AppendUint64(out, uint64(reqID))
	out = binary.BigEndian.AppendUint32(out, 531)
	out = binary.BigEndian.AppendUint32(out, yspQMFAppID)
	out = binary.BigEndian.AppendUint64(out, yspBizID)
	g := make([]byte, 32)
	copy(g, guid)
	out = append(out, g...)
	out = append(out, yspQMFPlatform)
	out = binary.BigEndian.AppendUint32(out, yspVersionCode)
	out = append(out, make([]byte, 6)...)
	out = append(out, 0)
	out = binary.BigEndian.AppendUint16(out, 0)
	out = binary.BigEndian.AppendUint16(out, 0)
	out = binary.BigEndian.AppendUint32(out, uint32(len(inner)))
	out = append(out, compressed.Bytes()...)
	out = append(out, 3)
	binary.BigEndian.PutUint32(out[1:5], uint32(len(out)))
	return out, nil
}

func unwrapJCE(data []byte) ([]byte, error) {
	if len(data) < 90 || data[0] != 0x13 {
		return nil, errors.New("invalid JCE envelope")
	}
	flags := binary.BigEndian.Uint32(data[21:25])
	payload := data[89 : len(data)-1]
	if flags&2 != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		payload, err = io.ReadAll(io.LimitReader(zr, 16<<20))
		closeErr := zr.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if len(payload) < 17 || payload[0] != '&' || payload[len(payload)-1] != '(' {
		return nil, errors.New("invalid JCE payload")
	}
	r := &jceReader{d: payload[16 : len(payload)-1]}
	m, err := r.structure()
	if err != nil {
		return nil, err
	}
	b, ok := m[1].([]byte)
	if !ok || len(b) == 0 {
		return nil, errors.New("empty JCE response body")
	}
	return b, nil
}

type deadHostError struct{ host string }

func (e *deadHostError) Error() string { return "dead CDN host: " + e.host }

func jceTimeshiftURL(ctx context.Context, client *http.Client, guid, pid, sid string, start, end int64, stream string) (string, error) {
	w := &jceWriter{}
	w.str(pid, 0)
	w.str(sid, 1)
	w.long(start, 2)
	w.long(end, 3)
	w.str(stream, 4)
	reqID := time.Now().UnixMilli() & 0x7fffffff
	packet, err := wrapJCE(pidTimeShift, w.b, reqID, guid)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://jacc.ysp.cctv.cn", bytes.NewReader(packet))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("jacc returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	respBody, err := unwrapJCE(raw)
	if err != nil {
		return "", err
	}
	r := &jceReader{d: respBody}
	m, err := r.structure()
	if err != nil {
		return "", err
	}
	code, _ := m[0].(int64)
	if code != 0 {
		msg, _ := m[1].(string)
		if msg == "" {
			msg = fmt.Sprintf("errCode=%d", code)
		}
		return "", errors.New(msg)
	}
	u, _ := m[2].(string)
	if u == "" {
		return "", errors.New("empty m3u8 URL")
	}
	if bytes.Contains([]byte(u), []byte("liverecord.video.cloud.cctv.com")) {
		return "", &deadHostError{host: "liverecord.video.cloud.cctv.com"}
	}
	return u, nil
}
