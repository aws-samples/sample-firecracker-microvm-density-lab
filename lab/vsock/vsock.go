// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package vsock holds the small pieces of the guest-agent vsock protocol that every
// host-side client must get right.
//
// Three clients speak this protocol (lab/sweep, host-agent/internal/orchestrator, and
// examples/dispatch), each deliberately carrying its own copy of the CONNECT dance so the
// examples stay self-explanatory. The one part they must share is the security-relevant
// part: how a reply line is read. The guest is the untrusted party in this design — a
// compromised guest agent can stream bytes forever without a newline, and an unbounded
// ReadString on the host would buffer all of it. ReadLine caps that.
package vsock

import (
	"bufio"
	"fmt"
)

// MaxReplyBytes is the largest reply line a host-side client accepts from a guest.
//
// Protocol replies are single lines of numeric fields (OK/OKC/OKS/OKT/OKE/ERR plus a
// handful of integers), tens of bytes in practice. 64 KiB is orders of magnitude above any
// legitimate reply while keeping the worst case a guest can pin per connection trivial.
const MaxReplyBytes = 64 * 1024

// ReadLine reads one \n-terminated line from br, rejecting lines longer than max bytes.
//
// It returns the line without the trailing newline. If the guest closes the connection or
// the deadline expires mid-line, the underlying read error is returned. If the line exceeds
// max before a newline arrives, ReadLine stops consuming and returns an error: the reply is
// malformed or hostile either way, and the caller should drop the connection.
func ReadLine(br *bufio.Reader, max int) (string, error) {
	if max <= 0 {
		max = MaxReplyBytes
	}
	buf := make([]byte, 0, 128)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return string(buf), nil
		}
		if len(buf) >= max {
			return "", fmt.Errorf("reply exceeded %d bytes without a newline; dropping hostile or corrupt guest reply", max)
		}
		buf = append(buf, b)
	}
}
