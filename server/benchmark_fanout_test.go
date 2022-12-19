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
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func BenchmarkFanout(b *testing.B) {

	type BenchmarkParameters struct {
		clusterSize      int
		numConnections   int
		numSubscriptions int
		subjectSize      int
		msgSize          int64
	}

	// Percent of messages published that each subscription must receive in order to pass the test
	const deliveryThresholdPercent = 95

	createFanoutBenchmark := func(bp BenchmarkParameters) (string, func(b *testing.B)) {
		name := fmt.Sprintf(
			"N=%d,Conn=%d,Sub=%d,SubjSz=%d,MsgSz=%d",
			bp.clusterSize,
			bp.numConnections,
			bp.numSubscriptions,
			bp.subjectSize,
			bp.msgSize,
		)

		subject := strings.Repeat("s", bp.subjectSize)

		return name, func(b *testing.B) {

			deliverThreshold := int64((b.N * deliveryThresholdPercent) / 100)

			// Create cluster with given number of servers
			cluster := createClusterWithName(b, "fanout-test", bp.clusterSize)
			for _, server := range cluster.servers {
				if err := server.readyForConnections(3 * time.Second); err != nil {
					b.Fatalf("timeout waiting for server: %v", err)
				}
			}
			defer cluster.shutdown()

			// Error handler that mutes the (expected) flood of nats.ErrSlowConsumer
			handleSubError := func(conn *nats.Conn, s *nats.Subscription, err error) {
				if err == nats.ErrSlowConsumer {
					// noop
				} else {
					b.Logf("%v", err)
				}
			}

			// Create connections
			connections := make([]*nats.Conn, bp.numConnections)
			for i := 0; i < bp.numConnections; i++ {
				nc, err := nats.Connect(cluster.randomServer().ClientURL(), nats.ErrorHandler(handleSubError))
				if err != nil {
					b.Fatalf("failed to connect: %v", err)
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
			subscriptions := make([]*nats.Subscription, bp.numSubscriptions)
			subscriptionCounters := make([]int64, bp.numSubscriptions)
			subscriptionDoneCh := make(chan bool, bp.numSubscriptions)

			createMessageHandler := func(subIndex int) func(msg *nats.Msg) {
				return func(msg *nats.Msg) {
					subscriptionCounters[subIndex] += 1
					if subscriptionCounters[subIndex] >= deliverThreshold {
						if msg.Sub != subscriptions[subIndex] {
							b.Fatalf("sub mismatch: %v != %v", msg.Sub, subscriptions[subIndex])
						}
						err := subscriptions[subIndex].Unsubscribe()
						if err != nil {
							b.Logf("Failed to unsubscribe: %v", err)
						}
						subscriptions[subIndex] = nil
						subscriptionDoneCh <- true
					}
				}
			}

			for i := 0; i < bp.numSubscriptions; i++ {
				nc := connections[i%bp.numConnections]
				sub, err := nc.Subscribe(subject, createMessageHandler(i))
				if err != nil {
					b.Fatalf("failed to subscribe: %v", err)
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
				b.Fatalf("failed to connect: %v", err)
			}
			defer nc.Close()

			// Start the benchmark
			b.ResetTimer()
			b.SetBytes(bp.msgSize)
			data := make([]byte, bp.msgSize)
			for i := 0; i < b.N; i++ {
				err := nc.Publish(subject, data)
				if err != nil {
					b.Fatalf("failed to publish: %v", err)
				}
			}

			subDoneCounter := 0
			for {
				select {
				case _ = <-subscriptionDoneCh:
					subDoneCounter += 1
					if subDoneCounter == bp.numSubscriptions {
						return
					}
				case <-time.After(3 * time.Second):
					b.Fatalf(
						"Timeout, %d/%d subscription received at least %d/%d messages",
						subDoneCounter,
						bp.numSubscriptions,
						deliverThreshold,
						b.N,
					)
				}
			}
		}
	}

	// Table of parametrized benchmarks
	testCases := []BenchmarkParameters{
		{clusterSize: 1, numConnections: 1, numSubscriptions: 1, subjectSize: 1, msgSize: 10},
		{clusterSize: 1, numConnections: 10, numSubscriptions: 10, subjectSize: 10, msgSize: 10},
		{clusterSize: 1, numConnections: 100, numSubscriptions: 100, subjectSize: 10, msgSize: 100},
		{clusterSize: 3, numConnections: 100, numSubscriptions: 100, subjectSize: 10, msgSize: 100},
		{clusterSize: 3, numConnections: 100, numSubscriptions: 1000, subjectSize: 10, msgSize: 100},
		{clusterSize: 3, numConnections: 100, numSubscriptions: 5000, subjectSize: 10, msgSize: 100},
		{clusterSize: 5, numConnections: 1000, numSubscriptions: 1000, subjectSize: 10, msgSize: 100},
		{clusterSize: 5, numConnections: 100, numSubscriptions: 100, subjectSize: 10, msgSize: 1000},
	}

	b.Logf("Fanout benchmark %d test cases", len(testCases))

	for _, testCase := range testCases {
		benchName, benchFun := createFanoutBenchmark(testCase)
		b.Run(benchName, benchFun)
	}
}
