package server

import "google.golang.org/grpc/codes"

// Unavailable reports a dependency or disabled runtime without contract details.
// The caller may retry: the handler has not committed an effect.
func Unavailable() error { return bare(codes.Unavailable) }
