// Copyright 2018 Drone.IO Inc.
// Copyright 2021 Informatyka Boguslawski sp. z o.o. sp.k., http://www.ib.pl/
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
//
// This file has been modified by Informatyka Boguslawski sp. z o.o. sp.k.

package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"github.com/woodpecker-ci/woodpecker/server"
	"github.com/woodpecker-ci/woodpecker/server/model"
	"github.com/woodpecker-ci/woodpecker/server/queue"
	"github.com/woodpecker-ci/woodpecker/server/remote"
	"github.com/woodpecker-ci/woodpecker/server/router/middleware/session"
	"github.com/woodpecker-ci/woodpecker/server/shared"
	"github.com/woodpecker-ci/woodpecker/server/store"
)

func GetBuilds(c *gin.Context) {
	repo := session.Repo(c)
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	builds, err := store.FromContext(c).GetBuildList(repo, page)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.JSON(http.StatusOK, builds)
}

func GetBuild(c *gin.Context) {
	store_ := store.FromContext(c)
	if c.Param("number") == "latest" {
		GetBuildLast(c)
		return
	}

	repo := session.Repo(c)
	num, err := strconv.ParseInt(c.Param("number"), 10, 64)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	files, _ := store_.FileList(build)
	procs, _ := store_.ProcList(build)
	build.Procs = model.Tree(procs)
	build.Files = files

	c.JSON(http.StatusOK, build)
}

func GetBuildLast(c *gin.Context) {
	store_ := store.FromContext(c)
	repo := session.Repo(c)
	branch := c.DefaultQuery("branch", repo.Branch)

	build, err := store_.GetBuildLast(repo, branch)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	procs, _ := store_.ProcList(build)
	build.Procs = model.Tree(procs)
	c.JSON(http.StatusOK, build)
}

func GetBuildLogs(c *gin.Context) {
	store_ := store.FromContext(c)
	repo := session.Repo(c)

	// parse the build number and job sequence number from
	// the request parameter.
	num, _ := strconv.ParseInt(c.Params.ByName("number"), 10, 64)
	ppid, _ := strconv.Atoi(c.Params.ByName("pid"))
	name := c.Params.ByName("proc")

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	proc, err := store_.ProcChild(build, ppid, name)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	rc, err := store_.LogFind(proc)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	defer rc.Close()

	c.Header("Content-Type", "application/json")
	io.Copy(c.Writer, rc)
}

func GetProcLogs(c *gin.Context) {
	store_ := store.FromContext(c)
	repo := session.Repo(c)

	// parse the build number and job sequence number from
	// the request parameter.
	num, _ := strconv.ParseInt(c.Params.ByName("number"), 10, 64)
	pid, _ := strconv.Atoi(c.Params.ByName("pid"))

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	proc, err := store_.ProcFind(build, pid)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	rc, err := store_.LogFind(proc)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	defer rc.Close()

	c.Header("Content-Type", "application/json")
	io.Copy(c.Writer, rc)
}

// DeleteBuild cancels a build
func DeleteBuild(c *gin.Context) {
	store_ := store.FromContext(c)
	repo := session.Repo(c)
	num, _ := strconv.ParseInt(c.Params.ByName("number"), 10, 64)

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		_ = c.AbortWithError(404, err)
		return
	}

	procs, err := store_.ProcList(build)
	if err != nil {
		_ = c.AbortWithError(404, err)
		return
	}

	if build.Status != model.StatusRunning && build.Status != model.StatusPending {
		c.String(400, "Cannot cancel a non-running or non-pending build")
		return
	}

	// First cancel/evict procs in the queue in one go
	var (
		procToCancel []string
		procToEvict  []string
	)
	for _, proc := range procs {
		if proc.PPID != 0 {
			continue
		}
		if proc.State == model.StatusRunning {
			procToCancel = append(procToCancel, fmt.Sprint(proc.ID))
		}
		if proc.State == model.StatusPending {
			procToEvict = append(procToEvict, fmt.Sprint(proc.ID))
		}
	}
	server.Config.Services.Queue.EvictAtOnce(context.Background(), procToEvict)
	server.Config.Services.Queue.ErrorAtOnce(context.Background(), procToEvict, queue.ErrCancel)
	server.Config.Services.Queue.ErrorAtOnce(context.Background(), procToCancel, queue.ErrCancel)

	// Then update the DB status for pending builds
	// Running ones will be set when the agents stop on the cancel signal
	for _, proc := range procs {
		if proc.State == model.StatusPending {
			if proc.PPID != 0 {
				if _, err = shared.UpdateProcToStatusSkipped(store_, *proc, 0); err != nil {
					log.Error().Msgf("error: done: cannot update proc_id %d state: %s", proc.ID, err)
				}
			} else {
				if _, err = shared.UpdateProcToStatusKilled(store_, *proc); err != nil {
					log.Error().Msgf("error: done: cannot update proc_id %d state: %s", proc.ID, err)
				}
			}
		}
	}

	killedBuild, err := shared.UpdateToStatusKilled(store_, *build)
	if err != nil {
		c.AbortWithError(500, err)
		return
	}

	// For pending builds, we stream the UI the latest state.
	// For running builds, the UI will be updated when the agents acknowledge the cancel
	if build.Status == model.StatusPending {
		procs, err = store_.ProcList(killedBuild)
		if err != nil {
			c.AbortWithError(404, err)
			return
		}
		killedBuild.Procs = model.Tree(procs)
		publishToTopic(c, killedBuild, repo, model.Cancelled)
	}

	c.String(204, "")
}

func PostApproval(c *gin.Context) {
	var (
		remote_ = remote.FromContext(c)
		store_  = store.FromContext(c)
		repo    = session.Repo(c)
		user    = session.User(c)
		num, _  = strconv.ParseInt(c.Params.ByName("number"), 10, 64)
	)

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}
	if build.Status != model.StatusBlocked {
		c.String(500, "cannot decline a build with status %s", build.Status)
		return
	}

	// fetch the build file from the database
	configs, err := server.Config.Storage.Config.ConfigsForBuild(build.ID)
	if err != nil {
		log.Error().Msgf("failure to get build config for %s. %s", repo.FullName, err)
		c.AbortWithError(404, err)
		return
	}

	netrc, err := remote_.Netrc(user, repo)
	if err != nil {
		c.String(500, "failed to generate netrc file. %s", err)
		return
	}

	if build, err = shared.UpdateToStatusPending(store_, *build, user.Login); err != nil {
		c.String(500, "error updating build. %s", err)
		return
	}

	c.JSON(200, build)

	// get the previous build so that we can send
	// on status change notifications
	last, _ := store_.GetBuildLastBefore(repo, build.Branch, build.ID)
	secs, err := server.Config.Services.Secrets.SecretListBuild(repo, build)
	if err != nil {
		log.Debug().Msgf("Error getting secrets for %s#%d. %s", repo.FullName, build.Number, err)
	}
	regs, err := server.Config.Services.Registries.RegistryList(repo)
	if err != nil {
		log.Debug().Msgf("Error getting registry credentials for %s#%d. %s", repo.FullName, build.Number, err)
	}
	envs := map[string]string{}
	if server.Config.Services.Environ != nil {
		globals, _ := server.Config.Services.Environ.EnvironList(repo)
		for _, global := range globals {
			envs[global.Name] = global.Value
		}
	}

	var yamls []*remote.FileMeta
	for _, y := range configs {
		yamls = append(yamls, &remote.FileMeta{Data: y.Data, Name: y.Name})
	}

	b := shared.ProcBuilder{
		Repo:  repo,
		Curr:  build,
		Last:  last,
		Netrc: netrc,
		Secs:  secs,
		Regs:  regs,
		Link:  server.Config.Server.Host,
		Yamls: yamls,
		Envs:  envs,
	}
	buildItems, err := b.Build()
	if err != nil {
		if _, err = shared.UpdateToStatusError(store_, *build, err); err != nil {
			log.Error().Msgf("Error setting error status of build for %s#%d. %s", repo.FullName, build.Number, err)
		}
		return
	}
	build = shared.SetBuildStepsOnBuild(b.Curr, buildItems)

	err = store_.ProcCreate(build.Procs)
	if err != nil {
		log.Error().Msgf("error persisting procs %s/%d: %s", repo.FullName, build.Number, err)
	}

	defer func() {
		for _, item := range buildItems {
			uri := fmt.Sprintf("%s/%s/%d", server.Config.Server.Host, repo.FullName, build.Number)
			if len(buildItems) > 1 {
				err = remote_.Status(c, user, repo, build, uri, item.Proc)
			} else {
				err = remote_.Status(c, user, repo, build, uri, nil)
			}
			if err != nil {
				log.Error().Msgf("error setting commit status for %s/%d: %v", repo.FullName, build.Number, err)
			}
		}
	}()

	publishToTopic(c, build, repo, model.Enqueued)
	queueBuild(build, repo, buildItems)
}

func PostDecline(c *gin.Context) {
	var (
		remote_ = remote.FromContext(c)
		store_  = store.FromContext(c)

		repo   = session.Repo(c)
		user   = session.User(c)
		num, _ = strconv.ParseInt(c.Params.ByName("number"), 10, 64)
	)

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}
	if build.Status != model.StatusBlocked {
		c.String(500, "cannot decline a build with status %s", build.Status)
		return
	}

	if _, err = shared.UpdateToStatusDeclined(store_, *build, user.Login); err != nil {
		c.String(500, "error updating build. %s", err)
		return
	}

	uri := fmt.Sprintf("%s/%s/%d", server.Config.Server.Host, repo.FullName, build.Number)
	err = remote_.Status(c, user, repo, build, uri, nil)
	if err != nil {
		log.Error().Msgf("error setting commit status for %s/%d: %v", repo.FullName, build.Number, err)
	}

	c.JSON(200, build)
}

func GetBuildQueue(c *gin.Context) {
	out, err := store.FromContext(c).GetBuildQueue()
	if err != nil {
		c.String(500, "Error getting build queue. %s", err)
		return
	}
	c.JSON(200, out)
}

// PostBuild restarts a build
func PostBuild(c *gin.Context) {
	remote_ := remote.FromContext(c)
	store_ := store.FromContext(c)
	repo := session.Repo(c)

	num, err := strconv.ParseInt(c.Param("number"), 10, 64)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	user, err := store_.GetUser(repo.UserID)
	if err != nil {
		log.Error().Msgf("failure to find repo owner %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		log.Error().Msgf("failure to get build %d. %s", num, err)
		c.AbortWithError(404, err)
		return
	}

	switch build.Status {
	case model.StatusDeclined,
		model.StatusBlocked:
		c.String(500, "cannot restart a build with status %s", build.Status)
		return
	}

	// if the remote has a refresh token, the current access token
	// may be stale. Therefore, we should refresh prior to dispatching
	// the job.
	if refresher, ok := remote_.(remote.Refresher); ok {
		ok, _ := refresher.Refresh(c, user)
		if ok {
			store_.UpdateUser(user)
		}
	}

	// fetch the pipeline config from database
	configs, err := server.Config.Storage.Config.ConfigsForBuild(build.ID)
	if err != nil {
		log.Error().Msgf("failure to get build config for %s. %s", repo.FullName, err)
		c.AbortWithError(404, err)
		return
	}

	netrc, err := remote_.Netrc(user, repo)
	if err != nil {
		log.Error().Msgf("failure to generate netrc for %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	build.ID = 0
	build.Number = 0
	build.Parent = num
	build.Status = model.StatusPending
	build.Started = 0
	build.Finished = 0
	build.Enqueued = time.Now().UTC().Unix()
	build.Error = ""
	build.Deploy = c.DefaultQuery("deploy_to", build.Deploy)

	event := c.DefaultQuery("event", build.Event)
	if event == model.EventPush ||
		event == model.EventPull ||
		event == model.EventTag ||
		event == model.EventDeploy {
		build.Event = event
	}

	err = store_.CreateBuild(build)
	if err != nil {
		c.String(500, err.Error())
		return
	}

	err = persistBuildConfigs(configs, build.ID)
	if err != nil {
		log.Error().Msgf("failure to persist build config for %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	// Read query string parameters into buildParams, exclude reserved params
	var buildParams = map[string]string{}
	for key, val := range c.Request.URL.Query() {
		switch key {
		case "fork", "event", "deploy_to":
		default:
			// We only accept string literals, because build parameters will be
			// injected as environment variables
			buildParams[key] = val[0]
		}
	}

	// get the previous build so that we can send
	// on status change notifications
	last, _ := store_.GetBuildLastBefore(repo, build.Branch, build.ID)
	secs, err := server.Config.Services.Secrets.SecretListBuild(repo, build)
	if err != nil {
		log.Debug().Msgf("Error getting secrets for %s#%d. %s", repo.FullName, build.Number, err)
	}
	regs, err := server.Config.Services.Registries.RegistryList(repo)
	if err != nil {
		log.Debug().Msgf("Error getting registry credentials for %s#%d. %s", repo.FullName, build.Number, err)
	}
	if server.Config.Services.Environ != nil {
		globals, _ := server.Config.Services.Environ.EnvironList(repo)
		for _, global := range globals {
			buildParams[global.Name] = global.Value
		}
	}

	var yamls []*remote.FileMeta
	for _, y := range configs {
		yamls = append(yamls, &remote.FileMeta{Data: y.Data, Name: y.Name})
	}

	b := shared.ProcBuilder{
		Repo:  repo,
		Curr:  build,
		Last:  last,
		Netrc: netrc,
		Secs:  secs,
		Regs:  regs,
		Link:  server.Config.Server.Host,
		Yamls: yamls,
		Envs:  buildParams,
	}
	buildItems, err := b.Build()
	if err != nil {
		build.Status = model.StatusError
		build.Started = time.Now().Unix()
		build.Finished = build.Started
		build.Error = err.Error()
		c.JSON(500, build)
		return
	}
	build = shared.SetBuildStepsOnBuild(b.Curr, buildItems)

	err = store_.ProcCreate(build.Procs)
	if err != nil {
		log.Error().Msgf("cannot restart %s#%d: %s", repo.FullName, build.Number, err)
		build.Status = model.StatusError
		build.Started = time.Now().Unix()
		build.Finished = build.Started
		build.Error = err.Error()
		c.JSON(500, build)
		return
	}
	c.JSON(202, build)

	publishToTopic(c, build, repo, model.Enqueued)
	queueBuild(build, repo, buildItems)
}

func DeleteBuildLogs(c *gin.Context) {
	store_ := store.FromContext(c)

	repo := session.Repo(c)
	user := session.User(c)
	num, _ := strconv.ParseInt(c.Params.ByName("number"), 10, 64)

	build, err := store_.GetBuildNumber(repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	procs, err := store_.ProcList(build)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	switch build.Status {
	case model.StatusRunning, model.StatusPending:
		c.String(400, "Cannot delete logs for a pending or running build")
		return
	}

	for _, proc := range procs {
		t := time.Now().UTC()
		buf := bytes.NewBufferString(fmt.Sprintf(deleteStr, proc.Name, user.Login, t.Format(time.UnixDate)))
		lerr := store_.LogSave(proc, buf)
		if lerr != nil {
			err = lerr
		}
	}
	if err != nil {
		c.String(400, "There was a problem deleting your logs. %s", err)
		return
	}

	c.String(204, "")
}

func persistBuildConfigs(configs []*model.Config, buildID int64) error {
	for _, conf := range configs {
		buildConfig := &model.BuildConfig{
			ConfigID: conf.ID,
			BuildID:  buildID,
		}
		err := server.Config.Storage.Config.BuildConfigCreate(buildConfig)
		if err != nil {
			return err
		}
	}
	return nil
}

var deleteStr = `[
	{
	  "proc": %q,
	  "pos": 0,
	  "out": "logs purged by %s on %s\n"
	}
]`
