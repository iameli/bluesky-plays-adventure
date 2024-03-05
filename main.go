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
	"time"

	"github.com/bluesky-social/indigo/api/atproto"
	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	appbsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/sequential"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/repo"
	"github.com/bluesky-social/indigo/util"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/gorilla/websocket"
	"github.com/ipfs/go-datastore"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipld/go-car"
)

type EmbedPost struct {
	CID    string
	Author string
	Path   string
}

func (p *EmbedPost) URI() string {
	// "uri": "at://did:plc:2zmxikig2sj7gqaezl5gntae/app.bsky.feed.post/3kmciya5y7k2l"
	return fmt.Sprintf("at://%s/%s", p.Author, p.Path)
}

var lastPost *EmbedPost

func main() {
	// err := Run(context.TODO())
	a, err := NewAdvent()
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			str := <-a.Output
			fmt.Println(str)
		}
	}()
	err = a.Start(context.TODO())
	if err != nil {
		panic(err)
	}
	fmt.Println("returned")
}

func Run(ctx context.Context) error {
	client, err := Client(ctx)
	if err != nil {
		return err
	}
	entry := make(chan string)
	go adventure(ctx, entry, client)
	err = Firehose(ctx, entry)
	if err != nil {
		return err
	}
	return nil
}

func Client(ctx context.Context) (*xrpc.Client, error) {
	anonClient := &xrpc.Client{
		Client: NewHttpClient(),
		Host:   "https://iame.li",
		Auth:   &xrpc.AuthInfo{},
	}
	session, err := comatproto.ServerCreateSession(ctx, anonClient, &comatproto.ServerCreateSession_Input{
		Identifier: "adventure.iame.li",
		Password:   os.Getenv("ADVENTURE_PASSWORD"),
	})
	if err != nil {
		return nil, err
	}
	return &xrpc.Client{
		Client: NewHttpClient(),
		Host:   "https://iame.li",
		Auth: &xrpc.AuthInfo{
			AccessJwt:  session.AccessJwt,
			RefreshJwt: session.RefreshJwt,
			Did:        session.Did,
			Handle:     session.Handle,
		},
	}, nil
}

func NewHttpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func MentionsMe(post *bsky.FeedPost) (bool, string) {
	if post.Reply != nil {
		if strings.Contains(post.Reply.Parent.Uri, "did:plc:4qqhm4o7ksjvz54ho3tutszn") {
			return true, post.Text
		}
	}
	for _, f := range post.Facets {
		for _, feat := range f.Features {
			if feat.RichtextFacet_Mention == nil {
				continue
			}
			if feat.RichtextFacet_Mention.Did == "did:plc:4qqhm4o7ksjvz54ho3tutszn" {
				bs := []byte(post.Text)
				before := bs[0:f.Index.ByteStart]
				after := bs[f.Index.ByteEnd:]
				removed := append([]byte{}, before...)
				removed = append(removed, after...)
				return true, string(removed)
			}
		}
	}
	return false, ""
}

func HandlePost(post *bsky.FeedPost, entry chan string, path, cid, author string) error {
	mentions, text := MentionsMe(post)
	if !mentions {
		return nil
	}
	text = strings.Trim(text, " ")
	lastPost = &EmbedPost{
		CID:    cid,
		Author: author,
		Path:   path,
	}
	fmt.Printf("got command: %s\n", text)
	entry <- fmt.Sprintf("%s\n", text)
	return nil
}

func Firehose(ctx context.Context, entry chan string) error {
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
					HandlePost(rec, entry, evt.Ops[0].Path, evt.Commit.String(), evt.Repo)
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

func adventure(ctx context.Context, entry chan string, client *xrpc.Client) error {
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
		first := true
		go func(i int, pipe io.ReadCloser) {
			reader := bufio.NewReader(pipe)
			for {
				line, err := reader.ReadString([]byte(">")[0])
				if err != nil {
					panic(err)
				}
				line = strings.TrimSuffix(line, ">")
				line = strings.TrimSpace(line)
				if first {
					first = false
					continue
				}
				fmt.Printf("Posting %q\n", line)
				err = post(ctx, line, client)
				if err != nil {
					fmt.Printf("error posting: %s\n", err)
				}
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

func post(ctx context.Context, text string, client *xrpc.Client) error {
	newpost := &appbsky.FeedPost{
		Text:      text,
		CreatedAt: time.Now().Format(util.ISO8601),
	}
	if lastPost != nil {
		newpost.Embed = &appbsky.FeedPost_Embed{
			EmbedRecord: &appbsky.EmbedRecord{
				LexiconTypeID: "app.bsky.embed.record",
				Record: &comatproto.RepoStrongRef{
					LexiconTypeID: "com.atproto.repo.strongRef",
					Cid:           lastPost.CID,
					Uri:           lastPost.URI(),
				},
			},
		}
	}
	_, err := comatproto.RepoCreateRecord(ctx, client, &comatproto.RepoCreateRecord_Input{
		Collection: "app.bsky.feed.post",
		Repo:       "did:plc:4qqhm4o7ksjvz54ho3tutszn",
		Record: &lexutil.LexiconTypeDecoder{
			Val: newpost,
		},
	})
	return err
}
