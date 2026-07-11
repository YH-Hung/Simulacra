package stub

import (
	"strings"
	"testing"
)

func TestCompileMetadataNormalizesKeysAndPreservesRawBinaryValues(t *testing.T) {
	raw := string([]byte{0x00, 0xff, 'x'})
	got, err := compileMetadata(map[string]string{
		"X-Mock":      "simulacra",
		"payload-BIN": raw,
	})
	if err != nil {
		t.Fatalf("compileMetadata: %v", err)
	}
	if values := got.Get("x-mock"); len(values) != 1 || values[0] != "simulacra" {
		t.Errorf("x-mock = %#v", values)
	}
	if values := got.Get("payload-bin"); len(values) != 1 || values[0] != raw {
		t.Errorf("payload-bin = %#v, want raw bytes %#v", values, raw)
	}
}

func TestCompileMetadataPreservesNilAndEmpty(t *testing.T) {
	nilMD, err := compileMetadata(nil)
	if err != nil {
		t.Fatal(err)
	}
	if nilMD != nil {
		t.Errorf("compileMetadata(nil) = %#v, want nil", nilMD)
	}
	emptyMD, err := compileMetadata(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if emptyMD == nil || len(emptyMD) != 0 {
		t.Errorf("compileMetadata(empty) = %#v, want non-nil empty MD", emptyMD)
	}
}

func TestCompileResponseMetadataRejectsInvalidKeysAndValuesWithContext(t *testing.T) {
	reg := testRegistry(t)
	tests := []struct {
		name     string
		trailers bool
		key      string
		value    string
	}{
		{name: "empty key", key: "", value: "x"},
		{name: "space in key", key: "bad key", value: "x"},
		{name: "pseudo key", key: ":status", value: "x"},
		{name: "non ASCII key", key: "méta", value: "x"},
		{name: "reserved prefix after normalization", key: "Grpc-Test", value: "x"},
		{name: "control value", key: "x-test", value: "line\nbreak"},
		{name: "non ASCII value", key: "x-test", value: string([]byte{0xff})},
		{name: "invalid trailer", trailers: true, key: "bad:key", value: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respond := Respond{Metadata: map[string]string{tt.key: tt.value}}
			where := "respond.metadata"
			if tt.trailers {
				respond = Respond{Trailers: map[string]string{tt.key: tt.value}}
				where = "respond.trailers"
			}
			_, err := Compile(reg, Stub{Method: "shop.v1.OrderService/GetOrder", Respond: respond}, "metadata.yaml#0")
			if err == nil {
				t.Fatal("Compile error = nil, want invalid metadata error")
			}
			if !strings.Contains(err.Error(), where) || !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q must mention %q and key %q", err, where, tt.key)
			}
		})
	}
}
