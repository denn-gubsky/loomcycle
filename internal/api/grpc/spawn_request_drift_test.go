package grpc

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

// populate sets every field of m to a non-zero value, recursing into messages
// (bounded, for recursive types). Bytes carry a JSON object, because the
// mappers decode metadata and schemas as one.
func populate(m protoreflect.Message, depth int) {
	if depth > 3 {
		return
	}
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		switch {
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			mp.Set(scalarValue(fd.MapKey()).MapKey(), mapValue(mp, fd.MapValue(), depth))
		case fd.IsList():
			list := m.Mutable(fd).List()
			if fd.Kind() == protoreflect.MessageKind {
				el := list.NewElement()
				populate(el.Message(), depth+1)
				list.Append(el)
			} else {
				list.Append(scalarValue(fd))
			}
		case fd.Kind() == protoreflect.MessageKind:
			populate(m.Mutable(fd).Message(), depth+1)
		default:
			m.Set(fd, scalarValue(fd))
		}
	}
}

func mapValue(mp protoreflect.Map, fd protoreflect.FieldDescriptor, depth int) protoreflect.Value {
	if fd.Kind() == protoreflect.MessageKind {
		v := mp.NewValue()
		populate(v.Message(), depth+1)
		return v
	}
	return scalarValue(fd)
}

func scalarValue(fd protoreflect.FieldDescriptor) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte(`{"k":"v"}`))
	case protoreflect.EnumKind:
		return protoreflect.ValueOfEnum(1)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(1)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(1)
	}
	panic("unhandled proto kind " + fd.Kind().String())
}

// spawnRequestFromProto builds the request behind CreateConfiguredRun and
// every SpawnRunBatch child. It dropped metadata, context, parent_context and
// interruption, which Run maps, so a configured run or a batch child silently
// lost them.
//
// The drift guard: runInputProtoArgs is exactly what Run maps out of a
// RunRequest. From a request with EVERY field set, spawnRequestFromProto must
// produce every one of those fields too. A field added to Run's mapping and
// not here fails this test instead of being dropped.
func TestSpawnRequestFromProto_MapsEveryFieldRunMaps(t *testing.T) {
	req := &loomcyclepb.RunRequest{}
	populate(req.ProtoReflect(), 0)
	got := reflect.ValueOf(spawnRequestFromProto(req))

	// Set by the caller rather than the mapper, each for a reason.
	callerSet := map[string]string{
		"Interactive": "only a configured run can park; CreateConfiguredRun sets it, a blocking batch child cannot",
	}
	args := reflect.TypeOf(runInputProtoArgs{})
	for i := 0; i < args.NumField(); i++ {
		name := args.Field(i).Name
		if _, ok := callerSet[name]; ok {
			continue
		}
		f := got.FieldByName(name)
		if !f.IsValid() {
			t.Errorf("Run maps %s, which connector.SpawnRunRequest has no field for", name)
			continue
		}
		if f.IsZero() {
			t.Errorf("Run maps %s but spawnRequestFromProto drops it (configured runs and batch children lose it)", name)
		}
	}
}
