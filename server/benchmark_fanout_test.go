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

	type subscriptionType string
	const (
		// Synchronous subscribe, does not consume any of the messages
		idleSync subscriptionType = "SynchronousIdle"
		// Asynchronous subscribe with default options and noop handler
		noopAsync subscriptionType = "AsynchronousNoop"
	)

	type TestParameters struct {
		clusterSize      int
		numConnections   int
		numSubscriptions int
		subjectSize      int
		msgSize          int64
		subType          subscriptionType
	}

	createFanoutBenchmark := func(p TestParameters) (string, func(b *testing.B)) {
		name := fmt.Sprintf(
			"SubType=%s,N=%d,Conn=%d,Sub=%d,SubjSz=%d,MsgSz=%d",
			p.subType,
			p.clusterSize,
			p.numConnections,
			p.numSubscriptions,
			p.subjectSize,
			p.msgSize,
		)

		subject := strings.Repeat("s", p.subjectSize)

		var subscribe func(nc *nats.Conn) (*nats.Subscription, error)
		switch p.subType {
		case idleSync:
			subscribe = func(nc *nats.Conn) (*nats.Subscription, error) {
				// Synchronous subscription, never consumed
				// Expect a lot of 'slow consumer' warnings
				return nc.SubscribeSync(subject)
			}
		case noopAsync:
			subscribe = func(nc *nats.Conn) (*nats.Subscription, error) {
				// Asynchronous NOOP subscription
				return nc.Subscribe(subject, func(msg *nats.Msg) {})
			}
		}

		return name, func(b *testing.B) {

			// Create cluster with given number of servers
			cluster := createClusterWithName(b, "fanout-test", p.clusterSize)
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
			connections := make([]*nats.Conn, p.numConnections)
			for i := 0; i < p.numConnections; i++ {
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
			subscriptions := make([]*nats.Subscription, p.numSubscriptions)
			for i := 0; i < p.numSubscriptions; i++ {
				nc := connections[i%p.numConnections]
				sub, err := subscribe(nc)
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
			b.SetBytes(p.msgSize)
			data := make([]byte, p.msgSize)
			for i := 0; i < b.N; i++ {
				err := nc.Publish(subject, data)
				if err != nil {
					b.Fatalf("failed to publish: %v", err)
				}
			}
		}
	}

	// Create tests table
	testCases := []TestParameters{}

	subscriberTypeCases := []subscriptionType{idleSync, noopAsync}
	for _, subscriberType := range subscriberTypeCases {
		testCases = append(testCases,
			TestParameters{clusterSize: 1, numConnections: 1, numSubscriptions: 1, subjectSize: 1, msgSize: 10, subType: subscriberType},
			TestParameters{clusterSize: 1, numConnections: 10, numSubscriptions: 10, subjectSize: 10, msgSize: 10, subType: subscriberType},
			TestParameters{clusterSize: 1, numConnections: 100, numSubscriptions: 100, subjectSize: 10, msgSize: 100, subType: subscriberType},
			TestParameters{clusterSize: 3, numConnections: 100, numSubscriptions: 100, subjectSize: 10, msgSize: 100, subType: subscriberType},
			TestParameters{clusterSize: 3, numConnections: 100, numSubscriptions: 1000, subjectSize: 10, msgSize: 100, subType: subscriberType},
			TestParameters{clusterSize: 3, numConnections: 1000, numSubscriptions: 1000, subjectSize: 10, msgSize: 1000, subType: subscriberType},
			TestParameters{clusterSize: 5, numConnections: 1000, numSubscriptions: 10000, subjectSize: 10, msgSize: 1000, subType: subscriberType},
		)
	}

	b.Logf("Fanout benchmark %d test cases", len(testCases))

	for _, testCase := range testCases {
		benchName, benchFun := createFanoutBenchmark(testCase)
		b.Run(benchName, benchFun)
	}
}
