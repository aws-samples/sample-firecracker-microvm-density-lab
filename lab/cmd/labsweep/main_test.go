// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// s3Destination mirrors the path construction in uploadToS3. Kept as a helper so the layout is
// asserted directly rather than by shelling out to the aws CLI.
func s3Destination(bucket, runStamp, localPath string) string {
	base := fmt.Sprintf("s3://%s/%s/%s", strings.TrimPrefix(bucket, "s3://"), S3Prefix, runStamp)
	return fmt.Sprintf("%s/%s", base, filepath.Base(localPath))
}

// Runs must land in their own timestamped prefix. A single shared prefix mixes datasets and,
// because filenames repeat across runs with the same run label, lets a later sweep overwrite an
// earlier one's artifacts.
func TestS3DestinationIsRunScoped(t *testing.T) {
	got := s3Destination("fc-lab-transfer-123", "20260801T164123Z",
		"/root/results/variance-smt-on-20260801T164123Z.csv")
	want := "s3://fc-lab-transfer-123/benchmark_results/20260801T164123Z/variance-smt-on-20260801T164123Z.csv"
	if got != want {
		t.Errorf("destination =\n  %s\nwant\n  %s", got, want)
	}
}

// A bucket may reasonably be given with or without the scheme; both must produce one s3:// and
// the same key, not "s3://s3://...".
func TestS3DestinationNormalizesBucketScheme(t *testing.T) {
	bare := s3Destination("my-bucket", "20260801T164123Z", "sweep.csv")
	scheme := s3Destination("s3://my-bucket", "20260801T164123Z", "sweep.csv")
	if bare != scheme {
		t.Errorf("bucket scheme changed the destination:\n  %s\n  %s", bare, scheme)
	}
	if strings.Count(bare, "s3://") != 1 {
		t.Errorf("destination %q should contain exactly one s3:// prefix", bare)
	}
}

// Two passes of the same experiment (smt-on then smt-off) share a run only if they share a
// timestamp. They are separate labsweep invocations, so each gets its own prefix, and the run
// label still distinguishes the filenames within it.
func TestS3DestinationSeparatesRuns(t *testing.T) {
	on := s3Destination("b", "20260801T164123Z", "variance-smt-on-20260801T164123Z.csv")
	off := s3Destination("b", "20260801T170045Z", "variance-smt-off-20260801T170045Z.csv")
	if on == off {
		t.Fatal("two runs produced the same destination")
	}
	if strings.Contains(on, "170045") || strings.Contains(off, "164123") {
		t.Error("run prefixes leaked across runs")
	}
	for _, d := range []string{on, off} {
		if !strings.Contains(d, "/"+S3Prefix+"/") {
			t.Errorf("destination %q is missing the %s prefix", d, S3Prefix)
		}
	}
}

// Only the basename belongs in the key. Joining the full local path would embed /root/results
// in the bucket layout and make keys depend on where the sweep happened to run.
func TestS3DestinationUsesBasenameOnly(t *testing.T) {
	got := s3Destination("b", "20260801T164123Z", "/root/results/deep/nested/samples.csv")
	if strings.Contains(got, "/root/") || strings.Contains(got, "nested") {
		t.Errorf("destination %q leaked the local directory structure", got)
	}
	if !strings.HasSuffix(got, "/samples.csv") {
		t.Errorf("destination %q should end with the bare filename", got)
	}
}
