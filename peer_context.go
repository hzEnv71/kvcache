package kvcache

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const peerMetaKey = "from_peer"

// 把 from_peer=true 写进 gRPC outgoing metadata
func peerContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, peerMetaKey, "true")
}

// 从 gRPC incoming metadata 里读 from_peer
func isFromPeer(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	vals := md.Get(peerMetaKey)
	return len(vals) > 0 && vals[0] == "true"
}
