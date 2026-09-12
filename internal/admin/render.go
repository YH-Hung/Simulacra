package admin

import (
	"sort"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// validUTF8 replaces each run of invalid UTF-8 in s with U+FFFD and returns
// valid input unchanged. Every string field the admin plane writes into a
// response passes through it (design §5.2): proto3 refuses to marshal a string
// holding invalid UTF-8, and one such string fails the whole response — a
// single bad header would fail ListCalls for the entire journal. The journal,
// store, and registry keep the original bytes; this is presentation only.
func validUTF8(s string) string { return strings.ToValidUTF8(s, "�") }

// renderCall turns a recorded call into its wire form (design §5.1). It never
// fails. Types resolve through the registry at render time, so a type
// registered after the call was recorded still renders.
func renderCall(call *journal.Call, types *schema.Types) *adminv1.Call {
	return &adminv1.Call{
		Seq:             call.Seq,
		Method:          validUTF8(call.Method),
		RequestMetadata: renderMetadata(call.Metadata),
		Requests:        renderMessages(call.Requests, types),
		Responses:       renderMessages(call.Responses, types),
		Status:          renderStatus(call, types),
		MatchedStubId:   validUTF8(call.StubID),
		Start:           timestamppb.New(call.Start),
		Duration:        durationpb.New(call.Duration),
	}
}

// renderMetadata emits one entry per key, keys sorted. The key suffix alone
// decides the field: "-bin" keys carry the bytes the application saw, and every
// other key carries text.
func renderMetadata(md metadata.MD) []*adminv1.MetadataEntry {
	keys := make([]string, 0, len(md))
	for key := range md {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]*adminv1.MetadataEntry, 0, len(keys))
	for _, key := range keys {
		entry := &adminv1.MetadataEntry{Key: validUTF8(key)}
		if strings.HasSuffix(strings.ToLower(key), "-bin") {
			for _, value := range md[key] {
				entry.BinaryValues = append(entry.BinaryValues, []byte(value))
			}
		} else {
			for _, value := range md[key] {
				entry.Values = append(entry.Values, validUTF8(value))
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func renderMessages(messages []*dynamicpb.Message, types *schema.Types) []*adminv1.DecodedMessage {
	decoded := make([]*adminv1.DecodedMessage, 0, len(messages))
	for _, message := range messages {
		if message != nil {
			decoded = append(decoded, decodeMessage(message, types))
		}
	}
	return decoded
}

// decodeMessage renders one message for a client that may not know its type.
// wire_bytes and json are each best effort: a field that cannot be produced is
// left empty rather than failing the RPC. Both marshals allow partial messages,
// because the journal holds whatever the data plane decoded. json uses proto
// field names because matcher paths do.
func decodeMessage(message proto.Message, types *schema.Types) *adminv1.DecodedMessage {
	decoded := &adminv1.DecodedMessage{
		TypeName: validUTF8(string(message.ProtoReflect().Descriptor().FullName())),
	}
	if wire, err := (proto.MarshalOptions{Deterministic: true, AllowPartial: true}).Marshal(message); err == nil {
		decoded.WireBytes = wire
	}
	text, err := (protojson.MarshalOptions{UseProtoNames: true, AllowPartial: true, Resolver: types}).Marshal(message)
	if err == nil {
		decoded.Json = validUTF8(string(text))
	}
	return decoded
}

// renderStatus always returns a status. The data plane records Err only on
// failure, so a nil Err is code 0.
func renderStatus(call *journal.Call, types *schema.Types) *adminv1.CallStatus {
	if call.Err == nil {
		return &adminv1.CallStatus{}
	}
	st := call.Err.Proto()
	rendered := &adminv1.CallStatus{Code: st.GetCode(), Message: validUTF8(st.GetMessage())}
	for _, detail := range st.GetDetails() {
		rendered.Details = append(rendered.Details, decodeDetail(detail, types))
	}
	return rendered
}

// decodeDetail resolves a status detail's type through the registry. A detail
// whose type does not resolve keeps its raw bytes and the type name its URL
// carries, with json empty.
func decodeDetail(detail *anypb.Any, types *schema.Types) *adminv1.DecodedMessage {
	if messageType, err := types.FindMessageByURL(detail.GetTypeUrl()); err == nil {
		message := messageType.New().Interface()
		if err := (proto.UnmarshalOptions{AllowPartial: true, Resolver: types}).Unmarshal(detail.GetValue(), message); err == nil {
			return decodeMessage(message, types)
		}
	}
	name := detail.GetTypeUrl()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return &adminv1.DecodedMessage{TypeName: validUTF8(name), WireBytes: detail.GetValue()}
}
