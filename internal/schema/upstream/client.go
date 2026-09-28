package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	v1pb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
)

// The two reflection method paths. We marshal grpc_reflection_v1 Go types
// against both: the v1 and v1alpha protos are wire-identical — v1 was copied
// from v1alpha and both are frozen — so one transport covers both versions
// and this codebase needs no v1alpha Go types at all (F1).
const (
	PathV1      = "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"
	PathV1Alpha = "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
)

// StreamOpener opens a bidirectional stream to one reflection method path.
// Production passes a closure over grpc.ClientConn.NewStream; tests pass a
// fake so the send/receive ordering is a parameter rather than a race (F2).
type StreamOpener func(ctx context.Context, path string) (grpc.ClientStream, error)

var streamDesc = &grpc.StreamDesc{
	StreamName:    "ServerReflectionInfo",
	ServerStreams: true,
	ClientStreams: true,
}

// NewFetcher returns a Fetcher over a reflection endpoint, and a release
// function the caller must call when it is done with the Fetcher (a
// deferred call is the usual shape).
//
// The protocol version is negotiated on the first call, over a single
// bidirectional ServerReflectionInfo stream that is then retained and reused
// for every subsequent request (design §5). That is a correctness property,
// not just resource hygiene: behind an L7 proxy (Envoy, nginx), successive
// streams can be routed to different backend instances, and if those
// backends run different service versions the walk would mix descriptors
// from both -- Closure's conflict detection would then blame "the upstream"
// for what is actually request routing. release cancels the retained
// stream's context; until it is called, the stream's goroutine and context
// are held open exactly as grpc.ClientConn.NewStream's doc warns.
//
// The returned Fetcher handles one call at a time and is not safe for
// concurrent use: reflectFetcher's fields are written without
// synchronization.
func NewFetcher(open StreamOpener) (f Fetcher, release func()) {
	r := &reflectFetcher{open: open}
	return r, r.release
}

type reflectFetcher struct {
	open StreamOpener
	// strm is the negotiated stream, retained and reused for every request
	// after the first. Nil until negotiate succeeds.
	strm grpc.ClientStream
	// cancel releases strm's context. Nil until strm is set.
	cancel context.CancelFunc
	// path is the negotiated method path, empty until the first successful
	// round trip.
	path string
}

// release cancels the retained stream's context, if one was ever
// negotiated. Safe to call more than once, and safe to call when no stream
// was ever opened (e.g. every call failed before negotiation completed).
func (r *reflectFetcher) release() {
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *reflectFetcher) ListServices(ctx context.Context) ([]string, error) {
	resp, err := r.roundTrip(ctx, &v1pb.ServerReflectionRequest{
		MessageRequest: &v1pb.ServerReflectionRequest_ListServices{ListServices: ""},
	})
	if err != nil {
		return nil, err
	}
	if err := inBandError(resp); err != nil {
		return nil, err
	}
	ls := resp.GetListServicesResponse()
	if ls == nil {
		return nil, fmt.Errorf("the upstream answered ListServices with %T",
			resp.GetMessageResponse())
	}
	out := make([]string, 0, len(ls.GetService()))
	for _, s := range ls.GetService() {
		out = append(out, s.GetName())
	}
	return out, nil
}

func (r *reflectFetcher) FileContainingSymbol(ctx context.Context, symbol string) ([][]byte, error) {
	return r.files(ctx, &v1pb.ServerReflectionRequest{
		MessageRequest: &v1pb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: symbol,
		},
	})
}

func (r *reflectFetcher) FileByFilename(ctx context.Context, name string) ([][]byte, error) {
	return r.files(ctx, &v1pb.ServerReflectionRequest{
		MessageRequest: &v1pb.ServerReflectionRequest_FileByFilename{FileByFilename: name},
	})
}

func (r *reflectFetcher) files(ctx context.Context, req *v1pb.ServerReflectionRequest) ([][]byte, error) {
	resp, err := r.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := inBandError(resp); err != nil {
		return nil, err
	}
	fdr := resp.GetFileDescriptorResponse()
	if fdr == nil {
		return nil, fmt.Errorf("the upstream answered with %T", resp.GetMessageResponse())
	}
	return fdr.GetFileDescriptorProto(), nil
}

// inBandError converts reflection's ErrorResponse into a real error.
// Reflection reports a missing file this way rather than failing the stream,
// so without this a NOT_FOUND would read as a successful empty result.
func inBandError(resp *v1pb.ServerReflectionResponse) error {
	e := resp.GetErrorResponse()
	if e == nil {
		return nil
	}
	return status.Error(codes.Code(e.GetErrorCode()), e.GetErrorMessage())
}

// roundTrip sends one request and reads one response over the retained
// stream, negotiating it first if this is the first call.
//
// Once negotiated, a failure is returned exactly as received: re-opening
// here would silently replay the request on a new stream, reintroducing the
// routing hazard retaining one stream in the first place removes.
func (r *reflectFetcher) roundTrip(ctx context.Context, req *v1pb.ServerReflectionRequest) (*v1pb.ServerReflectionResponse, error) {
	if r.strm != nil {
		return sendRecv(r.strm, req)
	}
	return r.negotiate(ctx, req)
}

// negotiate picks the protocol version on the first call: try v1, and if its
// first response is Unimplemented, fall back to v1alpha. The stream that
// answers successfully is retained in r.strm and r.cancel for every later
// roundTrip; the rejected v1 probe, if any, is abandoned -- its RecvMsg has
// already returned a non-nil error, so there is nothing further to read from
// it.
func (r *reflectFetcher) negotiate(ctx context.Context, req *v1pb.ServerReflectionRequest) (*v1pb.ServerReflectionResponse, error) {
	paths := []string{PathV1, PathV1Alpha}
	for i, path := range paths {
		// grpc.ClientConn.NewStream's doc lists four ways a stream's
		// resources are released — a canceled context is one — and warns
		// that its goroutine and context are leaked otherwise. The surviving
		// stream's context is canceled by release, once the whole walk is
		// done; a rejected probe's is canceled right here instead.
		streamCtx, cancel := context.WithCancel(ctx)
		strm, err := r.open(streamCtx, path)
		if err != nil {
			cancel()
			return nil, err
		}
		resp, err := sendRecv(strm, req)
		if err == nil {
			r.strm = strm
			r.cancel = cancel
			r.path = path
			return resp, nil
		}
		cancel()
		// Only an Unimplemented v1 earns a second attempt.
		if status.Code(err) == codes.Unimplemented && i < len(paths)-1 {
			continue
		}
		return nil, err
	}
	// Every branch above returns; the last path in paths never satisfies
	// i < len(paths)-1, so it always falls through to a return. Kept only
	// because a range loop is not itself a terminating statement.
	return nil, fmt.Errorf("internal: negotiate fell out of its retry loop over %v", paths)
}

// sendRecv performs one send/receive pair on an already-open stream.
func sendRecv(strm grpc.ClientStream, req *v1pb.ServerReflectionRequest) (*v1pb.ServerReflectionResponse, error) {
	// A send-side io.EOF means the stream is already done and carries no
	// status of its own; the real status comes from RecvMsg. Returning here
	// on any non-nil error — the obvious way to write it — would surface a
	// bare io.EOF and skip the fallback entirely against precisely the
	// servers the fallback exists for (F2).
	if err := strm.SendMsg(req); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	resp := &v1pb.ServerReflectionResponse{}
	if err := strm.RecvMsg(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Dial connects to an upstream. TLS with system roots by default, because
// "point at staging" is normally TLS; plaintext is the explicit opt-out that
// localhost and our own dogfood test need.
func Dial(addr string, plaintext bool) (*grpc.ClientConn, error) {
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if plaintext {
		creds = insecure.NewCredentials()
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

// OpenerFor adapts a connection into a StreamOpener.
func OpenerFor(cc *grpc.ClientConn) StreamOpener {
	return func(ctx context.Context, path string) (grpc.ClientStream, error) {
		return cc.NewStream(ctx, streamDesc, path)
	}
}
