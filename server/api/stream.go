// Copyright 2018 Drone.IO Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"github.com/woodpecker-ci/woodpecker/server"
	"github.com/woodpecker-ci/woodpecker/server/logging"
	"github.com/woodpecker-ci/woodpecker/server/model"
	"github.com/woodpecker-ci/woodpecker/server/pubsub"
	"github.com/woodpecker-ci/woodpecker/server/router/middleware/session"
	"github.com/woodpecker-ci/woodpecker/server/store"
)

//
// event source streaming for compatibility with quic and http2
//

func EventStreamSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	rw := c.Writer

	flusher, ok := rw.(http.Flusher)
	if !ok {
		c.String(500, "Streaming not supported")
		return
	}

	// ping the client
	io.WriteString(rw, ": ping\n\n")
	flusher.Flush()

	log.Debug().Msg("user feed: connection opened")

	user := session.User(c)
	repo := map[string]bool{}
	if user != nil {
		repos, _ := store.FromContext(c).RepoList(user, false)
		for _, r := range repos {
			repo[r.FullName] = true
		}
	}

	eventc := make(chan []byte, 10)
	ctx, cancel := context.WithCancel(
		context.Background(),
	)

	defer func() {
		cancel()
		close(eventc)
		log.Debug().Msg("user feed: connection closed")
	}()

	go func() {
		server.Config.Services.Pubsub.Subscribe(ctx, "topic/events", func(m pubsub.Message) {
			defer func() {
				recover() // fix #2480
			}()
			name := m.Labels["repo"]
			priv := m.Labels["private"]
			if repo[name] || priv == "false" {
				select {
				case <-ctx.Done():
					return
				default:
					eventc <- m.Data
				}
			}
		})
		cancel()
	}()

	for {
		select {
		case <-rw.CloseNotify():
			return
		case <-ctx.Done():
			return
		case <-time.After(time.Second * 30):
			io.WriteString(rw, ": ping\n\n")
			flusher.Flush()
		case buf, ok := <-eventc:
			if ok {
				io.WriteString(rw, "data: ")
				rw.Write(buf)
				io.WriteString(rw, "\n\n")
				flusher.Flush()
			}
		}
	}
}

func LogStreamSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	rw := c.Writer

	flusher, ok := rw.(http.Flusher)
	if !ok {
		c.String(500, "Streaming not supported")
		return
	}

	io.WriteString(rw, ": ping\n\n")
	flusher.Flush()

	repo := session.Repo(c)
	store_ := store.FromContext(c)

	// // parse the build number and job sequence number from
	// // the repquest parameter.
	buildn, _ := strconv.ParseInt(c.Param("build"), 10, 64)
	jobn, _ := strconv.Atoi(c.Param("number"))

	build, err := store_.GetBuildNumber(repo, buildn)
	if err != nil {
		log.Debug().Msgf("stream cannot get build number: %v", err)
		io.WriteString(rw, "event: error\ndata: build not found\n\n")
		return
	}
	proc, err := store_.ProcFind(build, jobn)
	if err != nil {
		log.Debug().Msgf("stream cannot get proc number: %v", err)
		io.WriteString(rw, "event: error\ndata: process not found\n\n")
		return
	}
	if proc.State != model.StatusRunning {
		log.Debug().Msg("stream not found.")
		io.WriteString(rw, "event: error\ndata: stream not found\n\n")
		return
	}

	logc := make(chan []byte, 10)
	ctx, cancel := context.WithCancel(
		context.Background(),
	)

	log.Debug().Msgf("log stream: connection opened")

	defer func() {
		cancel()
		close(logc)
		log.Debug().Msgf("log stream: connection closed")
	}()

	go func() {
		// TODO remove global variable
		server.Config.Services.Logs.Tail(ctx, fmt.Sprint(proc.ID), func(entries ...*logging.Entry) {
			defer func() {
				recover() // fix #2480
			}()
			for _, entry := range entries {
				select {
				case <-ctx.Done():
					return
				default:
					logc <- entry.Data
				}
			}
		})

		io.WriteString(rw, "event: error\ndata: eof\n\n")

		cancel()
	}()

	id := 1
	last, _ := strconv.Atoi(
		c.Request.Header.Get("Last-Event-ID"),
	)
	if last != 0 {
		log.Debug().Msgf("log stream: reconnect: last-event-id: %d", last)
	}

	// retry: 10000\n

	for {
		select {
		// after 1 hour of idle (no response) end the stream.
		// this is more of a safety mechanism than anything,
		// and can be removed once the code is more mature.
		case <-time.After(time.Hour):
			return
		case <-rw.CloseNotify():
			return
		case <-ctx.Done():
			return
		case <-time.After(time.Second * 30):
			io.WriteString(rw, ": ping\n\n")
			flusher.Flush()
		case buf, ok := <-logc:
			if ok {
				if id > last {
					io.WriteString(rw, "id: "+strconv.Itoa(id))
					io.WriteString(rw, "\n")
					io.WriteString(rw, "data: ")
					rw.Write(buf)
					io.WriteString(rw, "\n\n")
					flusher.Flush()
				}
				id++
			}
		}
	}
}
