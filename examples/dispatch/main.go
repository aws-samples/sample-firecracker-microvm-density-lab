// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Command dispatch sends one workload to a running Firecracker microVM's guest agent over
// vsock and prints the parsed reply.
//
// It demonstrates the guest-agent side of the loop from the architecture diagram: once the
// Host Agent has booted and pinned a microVM, work is dispatched to it over the guest agent's
// vsock protocol, and a result is read back.
//
// # Why a standalone vsock client rather than a host-agent Acquire client
//
// The Host Agent (../../host-agent) keeps its warm pool in-process: warmpool.Pool.Acquire
// hands a *orchestrator.MicroVM to callers inside the same binary, and the agent does not expose
// that pool over a network or Unix-socket RPC. There is therefore no wire API for an external
// process to acquire a microVM from a separately running host-agent.
//
// Rather than invent an RPC surface the host-agent does not have, this example talks the
// protocol the host-agent itself uses (orchestrator.AgentRequest) against a Firecracker vsock
// Unix socket you point it at with -vsock. That socket is the file the host-agent records as
// MicroVM.VsockUDS (<chroot>/v.sock) and is exactly what lab/sweep drives during a run. The wire
// contract is identical, so this example is a faithful stand-in for the dispatch step while
// depending on nothing that must be running as a server.
//
// # Usage
//
//	dispatch -vsock /srv/jailer/firecracker/vm0/root/v.sock                 # RUN_SANDBOX defaults
//	dispatch -vsock <uds> -workload sandbox -resident-kib 65536 -steps 20 -write-kib 256
//	dispatch -vsock <uds> -workload ping
//	dispatch -vsock <uds> -workload episode
//	dispatch -vsock <uds> -workload task
//	dispatch -vsock <uds> -raw 'RUN 1000000'                                # send any verb verbatim
//
// The default workload is RUN_SANDBOX, the agentic-sandbox-shaped workload from the guest agent:
// fork/exec churn, resident memory, and file writes.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/vsock"
)

// agentPort is the vsock port the guest agent listens on. It matches the guest-agent source,
// lab/sweep, and host-agent/internal/orchestrator.
const agentPort = 5000

func main() {
	var (
		vsockPath   = flag.String("vsock", "", "path to the Firecracker vsock Unix socket (the microVM's <chroot>/v.sock)")
		workload    = flag.String("workload", "sandbox", "workload: sandbox, ping, run, cache, task, episode")
		residentKiB = flag.Int("resident-kib", 65536, "RUN_SANDBOX resident working-set size in KiB")
		steps       = flag.Int("steps", 20, "RUN_SANDBOX fork/exec steps")
		writeKiB    = flag.Int("write-kib", 256, "RUN_SANDBOX bytes written per step, in KiB")
		iters       = flag.Int("iters", 1000000, "RUN / RUN_CACHE iteration count")
		wsKiB       = flag.Int("working-set-kib", 4096, "RUN_CACHE working-set size in KiB")
		raw         = flag.String("raw", "", "send this exact request line instead of building one from -workload")
		timeout     = flag.Duration("timeout", 60*time.Second, "request timeout")
	)
	flag.Parse()

	if *vsockPath == "" {
		fmt.Fprintln(os.Stderr, "error: -vsock is required (the microVM's <chroot>/v.sock path)")
		flag.Usage()
		os.Exit(2)
	}

	request := *raw
	if request == "" {
		var err error
		request, err = buildRequest(*workload, *residentKiB, *steps, *writeKiB, *iters, *wsKiB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(2)
		}
	}

	reply, err := agentRequest(*vsockPath, request, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dispatch failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("request:  %s\n", request)
	fmt.Printf("reply:    %s\n", reply)
	printParsed(reply)
}

// buildRequest turns a friendly workload name and its parameters into a protocol line.
func buildRequest(workload string, residentKiB, steps, writeKiB, iters, wsKiB int) (string, error) {
	switch strings.ToLower(workload) {
	case "sandbox":
		return fmt.Sprintf("RUN_SANDBOX %d %d %d", residentKiB, steps, writeKiB), nil
	case "ping":
		return "PING", nil
	case "run":
		return fmt.Sprintf("RUN %d", iters), nil
	case "cache":
		return fmt.Sprintf("RUN_CACHE %d %d", wsKiB, iters), nil
	case "task":
		return "RUN_TASK", nil
	case "episode":
		return "RUN_EPISODE", nil
	default:
		return "", fmt.Errorf("unknown workload %q (want sandbox, ping, run, cache, task, or episode)", workload)
	}
}

// agentRequest sends one line to a microVM's guest agent and returns its reply.
//
// This mirrors host-agent/internal/orchestrator.AgentRequest and lab/sweep's agentRequest:
// Firecracker bridges host AF_UNIX to guest AF_VSOCK, so we connect to the UDS, send
// "CONNECT <port>\n", read the "OK <hostport>" ack, then exchange one request/reply line on the
// same connection.
func agentRequest(vsockPath, request string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("unix", vsockPath, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial vsock uds %q: %w", vsockPath, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentPort); err != nil {
		return "", fmt.Errorf("send CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	// Bounded reads: the guest is the untrusted party, and an unbounded ReadString would
	// let a compromised agent stream newline-free bytes into host memory.
	ack, err := vsock.ReadLine(br, vsock.MaxReplyBytes)
	if err != nil {
		return "", fmt.Errorf("read CONNECT ack: %w", err)
	}
	if !strings.HasPrefix(ack, "OK") {
		// Firecracker closes the connection when nothing is listening on the port, so this
		// usually means the guest agent has not started yet.
		return "", fmt.Errorf("vsock connect refused: %q", strings.TrimSpace(ack))
	}

	if _, err := fmt.Fprintf(conn, "%s\n", request); err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	reply, err := vsock.ReadLine(br, vsock.MaxReplyBytes)
	if err != nil {
		return "", fmt.Errorf("read reply: %w", err)
	}
	return strings.TrimSpace(reply), nil
}

// printParsed decodes the reply into labelled fields when the tag is recognised. Field
// positions match the guest-agent protocol; each workload has its own tag because the fields
// differ and are not comparable across workloads.
func printParsed(reply string) {
	fields := strings.Fields(reply)
	if len(fields) == 0 {
		return
	}
	switch fields[0] {
	case "OKS":
		// OKS <resident_kib> <steps> <write_kib> <total_us> <spawn_us> <write_us> <spawns_ok> <checksum>
		//     <peak_rss_kib>
		printFields(fields, []string{
			"resident_kib", "steps", "write_kib", "total_us", "spawn_us", "write_us", "spawns_ok", "checksum",
			"peak_rss_kib",
		})
	case "OK":
		// PING -> "OK pong"; RUN -> OK <iterations> <total_us> <mean_ns> <checksum>
		if len(fields) == 2 && fields[1] == "pong" {
			fmt.Println("parsed:   PING acknowledged (guest agent is alive)")
			return
		}
		printFields(fields, []string{"iterations", "total_us", "mean_ns", "checksum"})
	case "OKC":
		// OKC <ws_kib> <iterations> <chase_us> <mean_ns> <setup_us> <checksum>
		printFields(fields, []string{"ws_kib", "iterations", "chase_us", "mean_ns", "setup_us", "checksum"})
	case "OKT":
		// OKT <exit_code> <total_us> <setup_us> <peak_rss_kib>
		printFields(fields, []string{"exit_code", "total_us", "setup_us", "peak_rss_kib"})
	case "OKE":
		// OKE <turns> <turns_ok> <total_us> <turns_us> <verify_us> <exit_code> <peak_rss_kib>
		printFields(fields, []string{"turns", "turns_ok", "total_us", "turns_us", "verify_us", "exit_code", "peak_rss_kib"})
	case "ERR":
		fmt.Printf("parsed:   guest reported an error: %s\n", strings.Join(fields[1:], " "))
	default:
		fmt.Printf("parsed:   unrecognised reply tag %q\n", fields[0])
	}
}

// printFields labels positional reply fields for human reading. Values after the tag are lined
// up with names; anything unlabelled is printed verbatim so nothing is silently dropped.
func printFields(fields []string, names []string) {
	fmt.Println("parsed:")
	values := fields[1:]
	for i, name := range names {
		if i >= len(values) {
			break
		}
		fmt.Printf("  %-12s %s\n", name, annotate(name, values[i]))
	}
	for i := len(names); i < len(values); i++ {
		fmt.Printf("  %-12s %s\n", fmt.Sprintf("extra[%d]", i), values[i])
	}
}

// annotate adds a derived, human-friendly rendering next to microsecond timings without
// hiding the raw value the guest reported.
func annotate(name, value string) string {
	if strings.HasSuffix(name, "_us") {
		if us, err := strconv.ParseFloat(value, 64); err == nil {
			return fmt.Sprintf("%s  (%.3f ms)", value, us/1000.0)
		}
	}
	return value
}
