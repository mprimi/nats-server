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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestIdleSynchronousSubscriptionOOM(t *testing.T) {
	const clusterSize = 1
	const numConnections = 1
	const numSubscriptions = 5_000
	const subjectLength = 10
	const msgSize = 10000
	const numMsg = 10_000_000

	subject := strings.Repeat("s", subjectLength)

	// Create cluster with given number of servers
	cluster := createClusterWithName(t, "sync_sub_oom", clusterSize)
	for _, server := range cluster.servers {
		if err := server.readyForConnections(3 * time.Second); err != nil {
			t.Fatalf("timeout waiting for server: %v", err)
		}
	}
	defer cluster.shutdown()

	// Error handler that mutes the (expected) flood of nats.ErrSlowConsumer
	handleSubError := func(conn *nats.Conn, s *nats.Subscription, err error) {
		if err == nats.ErrSlowConsumer {
			// noop
		} else {
			t.Logf("%v", err)
		}
	}

	// Create connections
	connections := make([]*nats.Conn, numConnections)
	for i := 0; i < numConnections; i++ {
		nc, err := nats.Connect(cluster.randomServer().ClientURL(), nats.ErrorHandler(handleSubError))
		if err != nil {
			t.Fatalf("failed to connect: %v", err)
		}
		connections[i] = nc
	}
	defer func() {
		for _, nc := range connections {
			if nc != nil {
				nc.Close()
			}
		}
	}()

	// Create subscriptions, round-robin over established connections
	subscriptions := make([]*nats.Subscription, numSubscriptions)
	for i := 0; i < numSubscriptions; i++ {
		nc := connections[i%numConnections]
		sub, err := nc.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("failed to subscribe: %v", err)
		}
		subscriptions[i] = sub
	}
	defer func() {
		for _, sub := range subscriptions {
			if sub != nil {
				_ = sub.Unsubscribe()
			}
		}
	}()

	// Create connection for publisher
	nc, err := nats.Connect(cluster.randomServer().ClientURL())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer nc.Close()

	ticker := time.NewTicker(1 * time.Second)

	data := make([]byte, msgSize)
	for i := 0; i < numMsg; i++ {
		err := nc.Publish(subject, data)
		if err != nil {
			t.Fatalf("failed to publish: %v", err)
		}

		select {
		case <-ticker.C:
			memStats := runtime.MemStats{}
			runtime.ReadMemStats(&memStats)
			t.Logf(
				"Published %d messages (%d MiB), runtime mem: %d MiB",
				i,
				(i*msgSize)/(1024*1024),
				memStats.Alloc/(1024*1024),
			)

		default:
			// Continue publishing
		}
	}
}
