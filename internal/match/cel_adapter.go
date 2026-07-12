package match

import (
	"fmt"
	"reflect"
	"strings"

	celtypes "github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

type resolverProvider struct {
	*celtypes.Registry
	files   *protoregistry.Files
	adapter *protoAdapter
}

func newCELProvider(files *protoregistry.Files) (*resolverProvider, error) {
	registry, err := celtypes.NewRegistry()
	if err != nil {
		return nil, err
	}
	provider := &resolverProvider{Registry: registry, files: files}
	var registerErr error
	files.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		registerErr = registry.RegisterDescriptor(file)
		return registerErr == nil
	})
	return provider, registerErr
}

func (p *resolverProvider) FindStructFieldType(structType, fieldName string) (*celtypes.FieldType, bool) {
	base, ok := p.Registry.FindStructFieldType(structType, fieldName)
	if !ok {
		return nil, false
	}
	descriptor, err := p.files.FindDescriptorByName(protoreflect.FullName(structType))
	if err != nil {
		return base, true
	}
	message, ok := descriptor.(protoreflect.MessageDescriptor)
	if !ok {
		return base, true
	}
	field := fieldByName(message, fieldName)
	if field == nil {
		return base, true
	}
	return &celtypes.FieldType{
		Type: base.Type,
		IsSet: func(target any) bool {
			value, ok := reflectedMessage(target)
			if !ok {
				return false
			}
			targetField := fieldByName(value.Descriptor(), fieldName)
			return targetField != nil && value.Has(targetField)
		},
		GetFrom: func(target any) (any, error) {
			value, ok := reflectedMessage(target)
			if !ok {
				return nil, fmt.Errorf("field %s.%s target has type %T, want protobuf message", structType, fieldName, target)
			}
			targetField := fieldByName(value.Descriptor(), fieldName)
			if targetField == nil {
				return nil, fmt.Errorf("message %s has no field %s", value.Descriptor().FullName(), fieldName)
			}
			fieldValue := value.Get(targetField)
			if targetField.Message() != nil && targetField.Message().FullName() == "google.protobuf.Any" && value.Has(targetField) {
				return p.adapter.unpackAny(fieldValue.Message())
			}
			return fieldValue.Interface(), nil
		},
		IsJSONField: base.IsJSONField,
	}, true
}

func reflectedMessage(value any) (protoreflect.Message, bool) {
	switch value := value.(type) {
	case proto.Message:
		return value.ProtoReflect(), true
	case protoreflect.Message:
		return value, true
	default:
		return nil, false
	}
}

type protoAdapter struct {
	fallback *celtypes.Registry
	dynamic  *dynamicpb.Types
}

func newProtoAdapter(provider *resolverProvider, files *protoregistry.Files) *protoAdapter {
	adapter := &protoAdapter{fallback: provider.Registry, dynamic: dynamicpb.NewTypes(files)}
	provider.adapter = adapter
	return adapter
}

func (a *protoAdapter) NativeToValue(value any) ref.Val {
	switch value := value.(type) {
	case proto.Message:
		message := value.ProtoReflect()
		if isOrdinaryDynamicMessage(message) {
			return &protoValue{adapter: a, message: message}
		}
	case protoreflect.Message:
		if isOrdinaryDynamicMessage(value) {
			return &protoValue{adapter: a, message: value}
		}
		return a.NativeToValue(value.Interface())
	case protoreflect.List:
		return celtypes.NewProtoList(a, value)
	case protoreflect.Map:
		entries := make(map[any]any, value.Len())
		value.Range(func(key protoreflect.MapKey, item protoreflect.Value) bool {
			entries[key.Interface()] = item.Interface()
			return true
		})
		return celtypes.NewDynamicMap(a, entries)
	case protoreflect.Value:
		return a.NativeToValue(value.Interface())
	}
	return a.fallback.NativeToValue(value)
}

func isOrdinaryDynamicMessage(message protoreflect.Message) bool {
	_, dynamic := message.Interface().(*dynamicpb.Message)
	return dynamic && !strings.HasPrefix(string(message.Descriptor().FullName()), "google.protobuf.")
}

type protoValue struct {
	adapter *protoAdapter
	message protoreflect.Message
}

func (v *protoValue) ConvertToNative(target reflect.Type) (any, error) {
	message := v.message.Interface()
	if reflect.TypeOf(message).AssignableTo(target) {
		return message, nil
	}
	if reflect.TypeOf(v).AssignableTo(target) {
		return v, nil
	}
	return nil, fmt.Errorf("type conversion error from %T to %v", message, target)
}

func (v *protoValue) ConvertToType(target ref.Type) ref.Val {
	if target.TypeName() == v.Type().TypeName() {
		return v
	}
	if target == celtypes.TypeType {
		return v.Type().(ref.Val)
	}
	return celtypes.NewErr("type conversion error from '%s' to '%s'", v.Type(), target)
}

func (v *protoValue) Equal(other ref.Val) ref.Val {
	message, ok := other.Value().(proto.Message)
	return celtypes.Bool(ok && proto.Equal(v.message.Interface(), message))
}

func (v *protoValue) Type() ref.Type {
	return celtypes.NewObjectType(string(v.message.Descriptor().FullName()), traits.IndexerType, traits.FieldTesterType)
}

func (v *protoValue) Value() any { return v.message.Interface() }

func (v *protoValue) Get(index ref.Val) ref.Val {
	name, ok := index.(celtypes.String)
	if !ok {
		return celtypes.MaybeNoSuchOverloadErr(index)
	}
	field := fieldByName(v.message.Descriptor(), string(name))
	if field == nil {
		return celtypes.NewErr("no such field '%s'", name)
	}
	value := v.message.Get(field)
	if field.Message() != nil && field.Message().FullName() == "google.protobuf.Any" && v.message.Has(field) {
		unpacked, err := v.adapter.unpackAny(value.Message())
		if err != nil {
			return celtypes.NewErr("unmarshal dynamic any failed: %v", err)
		}
		return v.adapter.NativeToValue(unpacked)
	}
	return v.adapter.NativeToValue(value)
}

func (v *protoValue) IsSet(index ref.Val) ref.Val {
	name, ok := index.(celtypes.String)
	if !ok {
		return celtypes.MaybeNoSuchOverloadErr(index)
	}
	field := fieldByName(v.message.Descriptor(), string(name))
	if field == nil {
		return celtypes.NewErr("no such field '%s'", name)
	}
	return celtypes.Bool(v.message.Has(field))
}

func (a *protoAdapter) unpackAny(envelope protoreflect.Message) (proto.Message, error) {
	typeURL := envelope.Get(envelope.Descriptor().Fields().ByName("type_url")).String()
	messageType, err := a.dynamic.FindMessageByURL(typeURL)
	if err != nil {
		messageType, err = protoregistry.GlobalTypes.FindMessageByURL(typeURL)
		if err != nil {
			return nil, fmt.Errorf("resolving %q: %w", typeURL, err)
		}
	}
	message := messageType.New().Interface()
	value := envelope.Get(envelope.Descriptor().Fields().ByName("value")).Bytes()
	if err := (proto.UnmarshalOptions{Resolver: a.dynamic}).Unmarshal(value, message); err != nil {
		return nil, fmt.Errorf("decoding %q: %w", typeURL, err)
	}
	return message, nil
}

func fieldByName(message protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	if field := message.Fields().ByName(protoreflect.Name(name)); field != nil {
		return field
	}
	fields := message.Fields()
	for i := 0; i < fields.Len(); i++ {
		if fields.Get(i).JSONName() == name {
			return fields.Get(i)
		}
	}
	return nil
}
