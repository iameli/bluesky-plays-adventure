package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/bluesky-social/indigo/api/atproto"
	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/sequential"
	"github.com/bluesky-social/indigo/repo"
	"github.com/gorilla/websocket"
	"github.com/ipfs/go-datastore"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipld/go-car"
)

func main() {
	entry := make(chan string)
	reader := bufio.NewReader(os.Stdin)
	go func() {
		fmt.Print("Enter text: ")
		text, _ := reader.ReadString('\n')
		entry <- text
	}()
	go adventure(entry)
	err := Firehose(context.TODO())
	if err != nil {
		panic(err)
	}
}

func MentionsMe(post *bsky.FeedPost) bool {
	for _, f := range post.Facets {
		for _, feat := range f.Features {
			if feat.RichtextFacet_Mention == nil {
				continue
			}
			if feat.RichtextFacet_Mention.Did == "did:plc:4qqhm4o7ksjvz54ho3tutszn" {
				return true
			}
		}
	}
	return false
}

func HandlePost(post *bsky.FeedPost) error {
	if !MentionsMe(post) {
		return nil
	}
	fmt.Printf("\tPost: %q\n", strings.Replace(post.Text, "\n", " ", -1))
	return nil
}

func Firehose(ctx context.Context) error {
	d := websocket.DefaultDialer
	con, _, err := d.Dial("wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos", http.Header{})
	if err != nil {
		return err
	}
	rsc := &events.RepoStreamCallbacks{
		RepoCommit: func(evt *comatproto.SyncSubscribeRepos_Commit) error {
			// fmt.Printf("(%d) RepoAppend: %s (%s)\n", evt.Seq, evt.Repo, evt.Commit.String())
			for _, op := range evt.Ops {
				if op.Action != "create" {
					return nil
				}
				if !strings.Contains(op.Path, "app.bsky.feed.post") {
					return nil
				}
			}
			recs, err := unpackRecords(evt.Blocks, evt.Ops)
			if err != nil {
				return err
			}

			for _, rec := range recs {
				switch rec := rec.(type) {
				case *bsky.FeedPost:
					HandlePost(rec)
				}
			}
			return nil
		},
		RepoMigrate: func(migrate *comatproto.SyncSubscribeRepos_Migrate) error {
			return nil
		},
		RepoHandle: func(handle *comatproto.SyncSubscribeRepos_Handle) error {
			return nil

		},
		RepoInfo: func(info *comatproto.SyncSubscribeRepos_Info) error {
			return nil
		},
		RepoTombstone: func(tomb *comatproto.SyncSubscribeRepos_Tombstone) error {
			return nil

		},
		// TODO: all the other event types
		Error: func(errf *events.ErrorFrame) error {
			return fmt.Errorf("error frame: %s: %s", errf.Error, errf.Message)
		},
	}
	seqScheduler := sequential.NewScheduler(con.RemoteAddr().String(), rsc.EventHandler)
	return events.HandleRepoStream(ctx, con, seqScheduler)
}

func unpackRecords(blks []byte, ops []*atproto.SyncSubscribeRepos_RepoOp) ([]any, error) {
	ctx := context.TODO()

	bstore := blockstore.NewBlockstore(datastore.NewMapDatastore())
	carr, err := car.NewCarReader(bytes.NewReader(blks))
	if err != nil {
		return nil, err
	}

	for {
		blk, err := carr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if err := bstore.Put(ctx, blk); err != nil {
			return nil, err
		}
	}

	r, err := repo.OpenRepo(ctx, bstore, carr.Header.Roots[0])
	if err != nil {
		return nil, err
	}

	var out []any
	for _, op := range ops {
		if op.Action == "create" {
			_, rec, err := r.GetRecord(ctx, op.Path)
			if err != nil {
				return nil, err
			}

			out = append(out, rec)
		}
	}

	return out, nil
}

func adventure(entry chan string) error {
	cmd := exec.Command("./adv550")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	go func(pipe io.WriteCloser) {
		for {
			line := <-entry
			pipe.Write([]byte(line))
		}
	}(stdin)

	for i, pipe := range []io.ReadCloser{stdout, stderr} {
		go func(i int, pipe io.ReadCloser) {
			reader := bufio.NewReader(pipe)
			for {
				line, _, _ := reader.ReadLine()
				fmt.Printf("%s\n", line)
			}
		}(i, pipe)
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		return err
	}
	return nil
}
