module github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent

go 1.23.12

require (
	github.com/aws-samples/sample-firecracker-microvm-density-lab/lab v0.0.0
	github.com/prometheus/client_golang v1.20.5
)

// The host agent reuses the lab's topology, placement, launcher, and firecracker
// packages unchanged. They live in the sibling lab/ module in this repository, so a
// replace directive points the import path at the local source rather than a published
// version. This is what lets the DaemonSet binary share the exact code the lab validated.
replace github.com/aws-samples/sample-firecracker-microvm-density-lab/lab => ../lab

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	golang.org/x/sys v0.22.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)
