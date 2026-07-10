package stub

import (
	"encoding/json"
	"fmt"

	_ "google.golang.org/genproto/googleapis/rpc/errdetails"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/yinghanhung/simulacra/internal/schema"
)

type StatusSpec struct {
	Code    string       `yaml:"code"`
	Message string       `yaml:"message"`
	Details []DetailSpec `yaml:"details"`
}

type DetailSpec struct {
	Type  string         `yaml:"type"`
	Value map[string]any `yaml:"value"`
}

func compileStatus(reg *schema.Registry, spec *StatusSpec, allowOK bool) (*status.Status, error) {
	if spec.Code == "" {
		return nil, fmt.Errorf("status code is required")
	}

	encodedCode, err := json.Marshal(spec.Code)
	if err != nil {
		return nil, fmt.Errorf("encoding status code %q: %w", spec.Code, err)
	}
	var code codes.Code
	if err := code.UnmarshalJSON(encodedCode); err != nil {
		return nil, fmt.Errorf("invalid status code %q: %w", spec.Code, err)
	}
	if code == codes.OK && !allowOK {
		return nil, fmt.Errorf("status code OK is not allowed here")
	}

	protoStatus := &spb.Status{Code: int32(code), Message: spec.Message}
	for i, detail := range spec.Details {
		desc, err := reg.LookupMessage(detail.Type)
		if err != nil {
			return nil, fmt.Errorf("details[%d] type %q: %w", i, detail.Type, err)
		}
		msg, err := BuildMessage(reg.Types(), desc, detail.Value)
		if err != nil {
			return nil, fmt.Errorf("details[%d] type %q: %w", i, detail.Type, err)
		}
		packed, err := anypb.New(msg)
		if err != nil {
			return nil, fmt.Errorf("details[%d] type %q: packing detail: %w", i, detail.Type, err)
		}
		protoStatus.Details = append(protoStatus.Details, packed)
	}

	return status.FromProto(protoStatus), nil
}
