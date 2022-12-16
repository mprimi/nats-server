// Copyright 2022 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// POC for how issue https://github.com/nats-io/nats.go/issues/1163 can affect server.
// Here a single NATS client can cause memory to build up in a cluster of JS servers.
// This is because messages are published with a given subject faster than the cluster can commit them to the stream.
func TestSlowConsumerOOM(t *testing.T) {
	const KiB = 1024
	const MiB = 1024 * KiB
	const GiB = 1024 * MiB

	const clusterSize = 5
	const subjectLength = 10
	const msgSize = 1 * MiB
	const numMsg = 10_000_000

	subject := strings.Repeat("s", subjectLength)
	t.Logf("Subject: %s", subject)

	// Create cluster with given number of servers
	cluster := createJetStreamClusterExplicit(t, "slow_consumer_OOM", clusterSize)
	for _, server := range cluster.servers {
		if err := server.readyForConnections(3 * time.Second); err != nil {
			t.Fatalf("timeout waiting for server: %v", err)
		}
	}
	defer cluster.shutdown()

	nc, err := nats.Connect(cluster.randomServer().ClientURL())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer nc.Close()

	// Create a stream with the given subject
	js, err := nc.JetStream()
	streamName := fmt.Sprintf("stream-%s", subject)
	streamCfg := nats.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
		MaxMsgs:  100, // Amount doesn't matter, just to avoid filling disk space
		Discard:  DiscardOld,
		Replicas: clusterSize,
	}
	_, err = js.AddStream(&streamCfg)
	if err != nil {
		t.Fatalf("Failed to create stream: %v", err)
	}

	// Create multiple connections to publish from (one per server to slow down and create more contention)
	connections := make([]*nats.Conn, clusterSize)
	for i, server := range cluster.servers {
		connections[i], err = nats.Connect(server.ClientURL())
		if err != nil {
			t.Fatalf("Failed to connect: %v", err)
		}
	}

	ticker := time.NewTicker(1 * time.Second)
	streamInfoTicker := time.NewTicker(3 * time.Second)

	data := make([]byte, msgSize)
	for i := 0; i < numMsg; i++ {
		err := connections[i%clusterSize].Publish(subject, data)
		if err != nil {
			t.Fatalf("failed to publish: %v", err)
		}

		select {
		case <-ticker.C:
			memStats := runtime.MemStats{}
			runtime.ReadMemStats(&memStats)
			t.Logf(
				"Published %d messages (%d MiB), runtime mem: %d GiB",
				i,
				(i*msgSize)/MiB,
				memStats.Alloc/GiB,
			)
		case <-streamInfoTicker.C:
			si, err := js.StreamInfo(streamName)
			if err != nil {
				t.Errorf("Failed to get stream %s info: %v", streamName, err)
			} else {
				ss := si.State
				t.Logf("Stream %s: %d messages %d MiB", streamName, ss.Msgs, ss.Bytes/MiB)
			}
		default:
			// Continue publishing
		}
	}
}
