package guest

import (
	"bytes"
	"compress/gzip"
	"testing"
)

// makeFakeZboot собирает имитацию zboot-обёртки: мусор + gzip-поток
// с «ядром» (магия ARM\x64 на 0x38) + мусор в хвосте.
func fakeZboot(t *testing.T, kernel []byte) []byte {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(kernel); err != nil {
		t.Fatal(err)
	}
	w.Close()
	out := append([]byte("MZ fake PE header..."), gz.Bytes()...)
	return append(out, []byte("trailing sections")...)
}

func fakeKernel() []byte {
	k := make([]byte, 1024)
	copy(k[0x38:0x3c], []byte{'A', 'R', 'M', 0x64})
	return k
}

func TestExtractARM64Image(t *testing.T) {
	kernel := fakeKernel()
	out, err := extractARM64Image(fakeZboot(t, kernel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, kernel) {
		t.Errorf("ядро не совпало: %d байт против %d", len(out), len(kernel))
	}
}

func TestExtractARM64ImageGarbage(t *testing.T) {
	// gzip-поток без магии ядра → ошибка.
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte("not a kernel at all, but a long enough string to pass 0x40 bytes check........"))
	w.Close()
	data := append(gz.Bytes(), []byte("junk")...)
	if _, err := extractARM64Image(data); err == nil {
		t.Error("ожидал ошибку на данных без ядра")
	}
	if _, err := extractARM64Image([]byte("no gzip at all")); err == nil {
		t.Error("ожидал ошибку на данных без gzip")
	}
}
