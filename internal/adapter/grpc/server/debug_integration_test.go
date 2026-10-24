package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type debugTransportService interface {
	Call(context.Context, proto.Message) (proto.Message, error)
}
type debugTransportHandler struct{ observed chan proto.Message }

func (h *debugTransportHandler) Call(_ context.Context, req proto.Message) (proto.Message, error) {
	h.observed <- proto.Clone(req)
	return req, nil
}

func debugTransportMessage(t *testing.T) *dynamicpb.Message {
	t.Helper()
	field := func(name string, n int32, kind descriptorpb.FieldDescriptorProto_Type, repeated bool, typeName string) *descriptorpb.FieldDescriptorProto {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		if repeated {
			label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		}
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Type: kind.Enum(), Label: label.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	str, blob, msg := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_BYTES, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	child := &descriptorpb.DescriptorProto{Name: proto.String("Child"), Field: []*descriptorpb.FieldDescriptorProto{field("access_token", 1, str, false, ""), field("payload", 2, blob, false, ""), field("label", 3, str, false, "")}}
	root := &descriptorpb.DescriptorProto{Name: proto.String("Request"), Field: []*descriptorpb.FieldDescriptorProto{field("refresh_token", 1, str, false, ""), field("payload", 2, blob, false, ""), field("tokens", 3, str, true, ""), field("files", 4, blob, true, ""), field("children", 5, msg, true, ".mask.Child"), field("child", 6, msg, false, ".mask.Child")}}
	for i, entry := range []struct {
		name, field string
		kind        descriptorpb.FieldDescriptorProto_Type
		typeName    string
	}{{"TokenMapEntry", "token_map", str, ""}, {"BytesMapEntry", "bytes_map", blob, ""}, {"ChildrenMapEntry", "children_map", msg, ".mask.Child"}} {
		root.NestedType = append(root.NestedType, &descriptorpb.DescriptorProto{Name: proto.String(entry.name), Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)}, Field: []*descriptorpb.FieldDescriptorProto{field("key", 1, str, false, ""), field("value", 2, entry.kind, false, entry.typeName)}})
		root.Field = append(root.Field, field(entry.field, int32(i+7), msg, true, ".mask.Request."+entry.name))
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{Name: proto.String("test/mask.proto"), Package: proto.String("mask"), Syntax: proto.String("proto3"), MessageType: []*descriptorpb.DescriptorProto{child, root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	childMsg := dynamicpb.NewMessage(fd.Messages().ByName("Child"))
	childMsg.Set(childMsg.Descriptor().Fields().ByName("access_token"), protoreflect.ValueOfString("PRIVATE-child-token"))
	childMsg.Set(childMsg.Descriptor().Fields().ByName("payload"), protoreflect.ValueOfBytes([]byte("PRIVATE-child-bytes")))
	childMsg.Set(childMsg.Descriptor().Fields().ByName("label"), protoreflect.ValueOfString("visible-label"))
	req := dynamicpb.NewMessage(fd.Messages().ByName("Request"))
	fields := req.Descriptor().Fields()
	req.Set(fields.ByName("refresh_token"), protoreflect.ValueOfString("PRIVATE-refresh-token"))
	req.Set(fields.ByName("payload"), protoreflect.ValueOfBytes([]byte("PRIVATE-payload")))
	req.Mutable(fields.ByName("tokens")).List().Append(protoreflect.ValueOfString("PRIVATE-list-token"))
	req.Mutable(fields.ByName("files")).List().Append(protoreflect.ValueOfBytes([]byte("PRIVATE-list-bytes")))
	req.Mutable(fields.ByName("children")).List().Append(protoreflect.ValueOfMessage(childMsg))
	req.Set(fields.ByName("child"), protoreflect.ValueOfMessage(proto.Clone(childMsg).ProtoReflect()))
	for name, value := range map[string]protoreflect.Value{"token_map": protoreflect.ValueOfString("PRIVATE-map-token"), "bytes_map": protoreflect.ValueOfBytes([]byte("PRIVATE-map-bytes")), "children_map": protoreflect.ValueOfMessage(proto.Clone(childMsg).ProtoReflect())} {
		req.Mutable(fields.ByName(protoreflect.Name(name))).Map().Set(protoreflect.ValueOfString("entry").MapKey(), value)
	}
	return req
}
func TestIntegrationDebugLogsMaskNestedWireMessagesWithoutMutatingPayload(t *testing.T) {
	req := debugTransportMessage(t)
	original := proto.Clone(req)
	var logs bytes.Buffer
	srv, err := New(Options{}, Deps{Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Metrics: prometheus.NewRegistry(), Services: DevServiceVerifier{}, Users: DevUserVerifier{}, Tombstones: NoTombstones{}, Policy: Policy{"/mask.Transport/Call": {Callers: []string{"bank-bff"}, Model: ModelUser}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := &debugTransportHandler{observed: make(chan proto.Message, 1)}
	srv.GRPC().RegisterService(&grpc.ServiceDesc{ServiceName: "mask.Transport", HandlerType: (*debugTransportService)(nil), Methods: []grpc.MethodDesc{{MethodName: "Call", Handler: func(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		message := dynamicpb.NewMessage(req.Descriptor())
		if err := decode(message); err != nil {
			return nil, err
		}
		next := func(ctx context.Context, input any) (any, error) {
			return service.(debugTransportService).Call(ctx, input.(proto.Message))
		}
		if interceptor == nil {
			return next(ctx, message)
		}
		return interceptor(ctx, message, &grpc.UnaryServerInfo{Server: service, FullMethod: "/mask.Transport/Call"}, next)
	}}}}, handler)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(srv.GRPC().Stop)
	conn, err := grpc.NewClient("passthrough:///debug-mask", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := call(t, "bank-bff", "u1", "idempotency-key", "PRIVATE-idempotency", "other-token", "PRIVATE-metadata-token", "private-bin", "PRIVATE-binary", "visible", "trace")
	response := dynamicpb.NewMessage(req.Descriptor())
	if err := conn.Invoke(ctx, "/mask.Transport/Call", req, response); err != nil {
		t.Fatal(err)
	}
	observed := <-handler.observed
	if !proto.Equal(req, original) || !proto.Equal(observed, original) || !proto.Equal(response, original) {
		t.Fatal("logging mutated actual request/response")
	}
	output := logs.String()
	for _, secret := range []string{"PRIVATE-payload", "PRIVATE-child-bytes", "PRIVATE-list-bytes", "PRIVATE-map-bytes", "PRIVATE-binary"} {
		if strings.Contains(output, base64.StdEncoding.EncodeToString([]byte(secret))) {
			t.Fatalf("base64bytes leaked in debuglog: %s", output)
		}
	}
	if strings.Contains(output, "PRIVATE") || strings.Contains(output, "dev:u1") || strings.Contains(output, "dev:bank-bff") {
		t.Fatalf("debuglog leaked credentials/data: %s", output)
	}
	if !strings.Contains(output, "visible-label") || !strings.Contains(output, "trace") || !strings.Contains(output, "***") || !strings.Contains(output, "(binary)") {
		t.Fatalf("masking lost permitted debug context: %s", output)
	}
}
