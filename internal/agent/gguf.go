package agent

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Reading a model's layer count from its GGUF header, so a split session
// can give the helpers only the share of layers their memory holds. A GGUF
// file starts with "GGUF", a version, the tensor count and a list of
// key/value pairs; the layer count is "<architecture>.block_count".

const (
	ggufMaxString = 1 << 24
	ggufMaxKVs    = 1 << 20
)

// ggufBlockCount returns the model's number of layers (transformer blocks).
func ggufBlockCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return 0, errors.New("not a GGUF file")
	}
	var version uint32
	var tensors, kvs uint64
	for _, v := range []any{&version, &tensors, &kvs} {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			return 0, err
		}
	}
	if version < 2 || version > 3 || kvs > ggufMaxKVs {
		return 0, fmt.Errorf("unsupported GGUF header (version %d, %d entries)", version, kvs)
	}
	for i := uint64(0); i < kvs; i++ {
		key, err := ggufString(r)
		if err != nil {
			return 0, err
		}
		var typ uint32
		if err := binary.Read(r, binary.LittleEndian, &typ); err != nil {
			return 0, err
		}
		if strings.HasSuffix(key, ".block_count") {
			n, err := ggufInt(r, typ)
			if err != nil {
				return 0, err
			}
			if n < 1 || n > 4096 {
				return 0, fmt.Errorf("odd layer count %d", n)
			}
			return int(n), nil
		}
		if err := ggufSkip(r, typ); err != nil {
			return 0, err
		}
	}
	return 0, errors.New("no layer count in the model header")
}

func ggufString(r *bufio.Reader) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > ggufMaxString {
		return "", errors.New("string too long")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return string(b), err
}

// ggufScalarSize is the byte size of a fixed-size value type, 0 for the rest.
func ggufScalarSize(typ uint32) int {
	switch typ {
	case 0, 1, 7: // uint8, int8, bool
		return 1
	case 2, 3: // uint16, int16
		return 2
	case 4, 5, 6: // uint32, int32, float32
		return 4
	case 10, 11, 12: // uint64, int64, float64
		return 8
	}
	return 0
}

func ggufInt(r *bufio.Reader, typ uint32) (int64, error) {
	n := ggufScalarSize(typ)
	if n == 0 || typ == 6 || typ == 12 || typ == 7 {
		return 0, fmt.Errorf("layer count has type %d", typ)
	}
	b := make([]byte, 8)
	if _, err := io.ReadFull(r, b[:n]); err != nil {
		return 0, err
	}
	u := binary.LittleEndian.Uint64(b)
	switch typ { // sign-extend the signed types
	case 1:
		return int64(int8(u)), nil
	case 3:
		return int64(int16(u)), nil
	case 5:
		return int64(int32(u)), nil
	}
	return int64(u), nil
}

func ggufSkip(r *bufio.Reader, typ uint32) error {
	if n := ggufScalarSize(typ); n > 0 {
		_, err := r.Discard(n)
		return err
	}
	switch typ {
	case 8: // string
		_, err := ggufString(r)
		return err
	case 9: // array: element type, count, elements
		var et uint32
		var count uint64
		if err := binary.Read(r, binary.LittleEndian, &et); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return err
		}
		if n := ggufScalarSize(et); n > 0 {
			_, err := r.Discard(int(count) * n)
			return err
		}
		for i := uint64(0); i < count; i++ {
			if err := ggufSkip(r, et); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown GGUF value type %d", typ)
}

// offloadLayers is the -ngl for the main device: how many of the model's
// layers the helpers take. The helpers get a share of the layers in
// proportion to the memory they can offer, the main device keeps the rest.
// helperBytes 0 means unknown, and everything goes to the helpers as before.
func offloadLayers(modelBytes int64, blocks int, helperBytes int64) int {
	const all = 999
	if helperBytes <= 0 || blocks <= 0 || modelBytes <= 0 || helperBytes >= modelBytes {
		return all
	}
	n := int(float64(blocks) * float64(helperBytes) / float64(modelBytes))
	return max(1, n)
}
