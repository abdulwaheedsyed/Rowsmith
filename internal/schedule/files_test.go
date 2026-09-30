package schedule

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func seal(t *testing.T, data []byte, key []byte, ad string) []byte {
	var out bytes.Buffer
	w, err := SealWriter(&out, key, ad)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(data); i += 7777 { // uneven writes
		end := min(i+7777, len(data))
		if _, err := w.Write(data[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func open(sealed, key []byte, ad string) ([]byte, error) {
	r, err := OpenReader(bytes.NewReader(sealed), key, ad)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestSealedFileRoundTrip(t *testing.T) {
	key := NewFileKey()
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3 * chunkSize, 3*chunkSize + 123} {
		data := make([]byte, size)
		rand.Read(data)
		sealed := seal(t, data, key, "run:1")
		got, err := open(sealed, key, "run:1")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: err=%v equal=%v", size, err, bytes.Equal(got, data))
		}
	}
}

func TestSealedFileDetectsTampering(t *testing.T) {
	key := NewFileKey()
	data := make([]byte, 3*chunkSize+100) // three full chunks and a short final one
	rand.Read(data)
	sealed := seal(t, data, key, "run:1")
	full := chunkSize + 16
	head := 1 + prefixSize
	cases := map[string][]byte{
		"flipped bit":      func() []byte { b := bytes.Clone(sealed); b[head+100] ^= 1; return b }(),
		"cut at chunk":     sealed[:head+2*full],
		"cut mid chunk":    sealed[:head+full+500],
		"final chunk gone": sealed[:head+3*full],
		"chunks swapped":   append(append(append([]byte{}, sealed[:head]...), sealed[head+full:head+2*full]...), append(append([]byte{}, sealed[head:head+full]...), sealed[head+2*full:]...)...),
		"empty":            {},
	}
	for name, b := range cases {
		if _, err := open(b, key, "run:1"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := open(sealed, key, "run:2"); err == nil {
		t.Error("another record's AD should not open the file")
	}
	if _, err := open(sealed, NewFileKey(), "run:1"); err == nil {
		t.Error("another key should not open the file")
	}
}
