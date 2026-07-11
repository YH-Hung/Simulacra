package stub

import (
	"slices"
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

func TestCompileResponseMetadataPreservesCaseCollisions(t *testing.T) {
	reg := testRegistry(t)
	compiled, err := Compile(reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Respond: Respond{
			Metadata: map[string]string{"X-Mock": "header-one", "x-mock": "header-two"},
			Trailers: map[string]string{"X-Trail": "trailer-one", "x-trail": "trailer-two"},
		},
	}, "metadata.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	tests := []struct {
		name   string
		values []string
		want   []string
	}{
		{name: "header", values: compiled.Plan().Header.Get("x-mock"), want: []string{"header-one", "header-two"}},
		{name: "trailer", values: compiled.Plan().Trailer.Get("x-trail"), want: []string{"trailer-one", "trailer-two"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.values) != len(tt.want) {
				t.Fatalf("values = %#v, want both %#v", tt.values, tt.want)
			}
			for _, value := range tt.want {
				if !slices.Contains(tt.values, value) {
					t.Errorf("values = %#v, missing %q", tt.values, value)
				}
			}
		})
	}
}

func TestCompileResponseMetadataRejectsTransportReservedNames(t *testing.T) {
	reg := testRegistry(t)
	reserved := []string{
		"Content-Type",
		"user-agent",
		"te",
		"grpc-status",
		"grpc-message",
		"grpc-timeout",
		"grpc-encoding",
		"grpc-message-type",
	}
	for _, key := range reserved {
		for _, location := range []string{"metadata", "trailers"} {
			t.Run(location+"/"+key, func(t *testing.T) {
				respond := Respond{Metadata: map[string]string{key: "reserved"}}
				if location == "trailers" {
					respond = Respond{Trailers: map[string]string{key: "reserved"}}
				}
				_, err := Compile(reg, Stub{Method: "shop.v1.OrderService/GetOrder", Respond: respond}, "reserved.yaml#0")
				if err == nil {
					t.Fatal("Compile error = nil, want reserved metadata name error")
				}
				where := "respond." + location
				if !strings.Contains(err.Error(), where) || !strings.Contains(err.Error(), key) {
					t.Errorf("error %q must mention %q and key %q", err, where, key)
				}
			})
		}
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
