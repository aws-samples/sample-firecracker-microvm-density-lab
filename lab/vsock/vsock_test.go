// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package vsock

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestReadLineReturnsLineWithoutNewline(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("OK pong\nleftover"))
	got, err := ReadLine(br, MaxReplyBytes)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if got != "OK pong" {
		t.Fatalf("got %q, want %q", got, "OK pong")
	}
	// The reader must not consume past the newline.
	rest, _ := io.ReadAll(br)
	if string(rest) != "leftover" {
		t.Fatalf("consumed past newline; remaining %q", rest)
	}
}

func TestReadLineEmptyLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\n"))
	got, err := ReadLine(br, MaxReplyBytes)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty line", got)
	}
}

func TestReadLineRejectsOversizedLine(t *testing.T) {
	// A hostile guest streams bytes with no newline: the reader must stop at the cap
	// rather than buffering indefinitely.
	hostile := strings.Repeat("A", MaxReplyBytes+1)
	br := bufio.NewReader(strings.NewReader(hostile))
	if _, err := ReadLine(br, MaxReplyBytes); err == nil {
		t.Fatal("expected an error for an oversized line, got nil")
	}
}

func TestReadLineAcceptsLineAtCap(t *testing.T) {
	line := strings.Repeat("B", 1024)
	br := bufio.NewReader(strings.NewReader(line + "\n"))
	got, err := ReadLine(br, 1024)
	if err != nil {
		t.Fatalf("ReadLine at cap: %v", err)
	}
	if got != line {
		t.Fatalf("line mangled at cap boundary")
	}
}

func TestReadLinePropagatesEOFMidLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("no newline"))
	if _, err := ReadLine(br, MaxReplyBytes); err != io.EOF {
		t.Fatalf("want io.EOF for truncated line, got %v", err)
	}
}

func TestReadLineZeroMaxUsesDefault(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("OK\n"))
	got, err := ReadLine(br, 0)
	if err != nil || got != "OK" {
		t.Fatalf("got %q, %v", got, err)
	}
}
