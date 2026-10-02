module github.com/aws-samples/sample-firecracker-microvm-density-lab/examples/dispatch

go 1.23.12

// The dispatch example speaks the guest-agent vsock protocol directly. It imports the lab
// module's vsock package for the bounded reply reader every host-side client must share
// (the guest is untrusted; replies are read with a hard length cap).
//
// The replace directives below resolve the sibling modules offline from the local source
// tree, matching how host-agent/ points at ../lab.
require github.com/aws-samples/sample-firecracker-microvm-density-lab/lab v0.0.0

replace github.com/aws-samples/sample-firecracker-microvm-density-lab/lab => ../../lab

replace github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent => ../../host-agent
