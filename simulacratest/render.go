package simulacratest

import (
	"bytes"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// renderMessage renders msg as a stub message body: protobuf JSON with field
// names as written in the .proto, enums by name, 64-bit integers as strings
// and well-known types in their JSON forms. It resolves through r, so
// extensions and Any payloads from registered files render too. Decoding with
// UseNumber keeps every number as protojson spelled it, and re-encoding the
// tree drops the whitespace protojson deliberately varies, so the document is
// stable.
func renderMessage(msg proto.Message, r chainResolver) (map[string]any, error) {
	b, err := protojson.MarshalOptions{UseProtoNames: true, Resolver: r}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	tree, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s renders as a JSON %s, not an object; respond.message must be a mapping, so this value cannot be expressed in the stub grammar", msg.ProtoReflect().Descriptor().FullName(), jsonKind(v))
	}
	return tree, nil
}

// jsonKind names the JSON type of a value decoded with UseNumber.
func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "bool"
	case []any:
		return "array"
	}
	return "null"
}

// guardRoundTrip reproduces the server's decode of a message body — its YAML
// loader reads the body as a value nested in the document, into
// map[string]any, and stub.BuildMessage re-encodes that with encoding/json
// for protojson to decode into the method's output type — and requires the
// result to be msg exactly. The path loses what the grammar cannot carry:
// protojson drops unknown fields, and YAML reads protojson's -0 as the integer
// 0, the one loss the YAML hop adds. The SDK refuses such a message rather
// than respell it, so the stub never answers with anything but what the test
// passed.
//
// It compares by proto.Equal and by deterministic bytes, because each misses
// something: proto.Equal compares floats with ==, so -0 in a Struct number is
// equal to the 0 that comes back, while the encodings keep the sign bit.
func guardRoundTrip(tree map[string]any, msg proto.Message, r chainResolver) error {
	body, err := json.Marshal(tree)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(`{"message": `+string(body)+`}`), &doc); err != nil {
		return err
	}
	data, err := json.Marshal(doc["message"])
	if err != nil {
		return err
	}
	got := msg.ProtoReflect().New()
	if err := (protojson.UnmarshalOptions{Resolver: r}).Unmarshal(data, got.Interface()); err != nil {
		return err
	}
	if err := checkExtensionInstances(got); err != nil {
		return err
	}
	det := proto.MarshalOptions{Deterministic: true}
	gotBytes, err := det.Marshal(got.Interface())
	if err != nil {
		return err
	}
	wantBytes, err := det.Marshal(msg)
	if err != nil {
		return err
	}
	if !proto.Equal(got.Interface(), msg) || !bytes.Equal(gotBytes, wantBytes) {
		return fmt.Errorf("%s does not survive the stub grammar's JSON round trip unchanged (unknown fields are dropped and negative zero loses its sign, for example); use StubYAML for full control", got.Descriptor().FullName())
	}
	return nil
}
