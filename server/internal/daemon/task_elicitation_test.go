package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskElicitationForwardAndCancel(t *testing.T) {
	for _, mode := range []string{"respond", "cancel", "old-server"} {
		t.Run(mode, func(t *testing.T) {
			var created, cancelled, polled atomic.Int32
			ready := make(chan struct{}, 1)
			d := taskSupplementTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					cancelled.Add(1)
					w.Write([]byte(`{}`))
					return
				}
				if r.Method == "POST" {
					created.Add(1)
					if mode == "old-server" {
						w.WriteHeader(404)
						return
					}
					w.WriteHeader(201)
					w.Write([]byte(`{}`))
					return
				}
				polled.Add(1)
				select {
				case ready <- struct{}{}:
				default:
				}
				if mode == "respond" {
					w.Write([]byte(`{"status":"responded","response":{"action":"decline"}}`))
				} else {
					w.Write([]byte(`{"status":"pending"}`))
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "cancel" {
				go func() {
					select {
					case <-ready:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			response, err := d.requestTaskElicitation(ctx, "fixture-task", json.RawMessage(`{"message":"original"}`), d.logger)
			if created.Load() != 1 || cancelled.Load() != 1 {
				t.Fatalf("create=%d cleanup=%d", created.Load(), cancelled.Load())
			}
			if mode == "respond" {
				if err != nil || string(response) != `{"action":"decline"}` {
					t.Fatal(string(response), err)
				}
			} else if err == nil {
				t.Fatal("must expose failure")
			}
		})
	}
}
