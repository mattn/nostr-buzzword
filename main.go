package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ikawaha/kagome-dict-ipa-neologd"
	"github.com/ikawaha/kagome-dict/dict"
	"github.com/ikawaha/kagome/v2/tokenizer"
	nostr "github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const name = "nostr-buzzword"

const version = "0.0.6"

var revision = "HEAD"

var (
	reLink = regexp.MustCompile(`\b\w+://\S+\b`)
	reTag  = regexp.MustCompile(`\b(\B#\S+|nostr:\S+)`)

	relays = []string{
		"wss://relay-jp.nostr.wirednet.jp",
		"wss://yabu.me",
		"wss://relay.nostr.band",
		"wss://nos.lol",
	}

	ignores = []string{
		"npub150qnaaxfan8auqdajvn292cgk2khm3tl8dmakamj878z44a6yntqk7uktv", // 流速ちゃｎ
		"npub1f6rvmwc76arl7sxx2vparlzx8cg2ajc3xpymqh7yx97znccue2hs5mkavc", // ぬるぽ
		"npub1w7g33p5hrljhnl37f7gdnhr7j87dwjzsms59x6qllutk2jepszgs65t8dc", // ビットコイン
		"npub17a50460j8y99yglsqzjzfh4exq4f8q0r82ackzzv4pz0dyd3rnwsxc9tp2", // buzzword
	}
)

func normalize(s string) string {
	s = reLink.ReplaceAllString(s, "")
	s = reTag.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func isIgnore(d *dict.Dict, c []string) bool {
	if len(c) == 0 {
		return true
	}
	if c[0] != "名詞" && c[0] != "副詞" {
		return true
	}
	if c[0] == "名詞" && c[1] != "一般" && c[1] != "固有名詞" {
		return true
	}
	return false
}

func postEvent(nsec string, relays []string, ev *nostr.Event, content string) error {
	eev := nostr.Event{}
	var sk string
	if _, s, err := nip19.Decode(nsec); err != nil {
		return err
	} else {
		sk = s.(string)
	}
	if pub, err := nostr.GetPublicKey(sk); err == nil {
		if _, err := nip19.EncodePublicKey(pub); err != nil {
			return err
		}
		eev.PubKey = pub
	} else {
		return err
	}

	eev.Content = content
	if ev != nil {
		eev.CreatedAt = ev.CreatedAt + 1
		eev.Kind = ev.Kind
		eev.Tags = eev.Tags.AppendUnique(nostr.Tag{"e", ev.ID, "", "reply"})
		eev.Tags = eev.Tags.AppendUnique(nostr.Tag{"p", ev.ID})
		for _, te := range ev.Tags {
			if te.Key() == "e" {
				eev.Tags = eev.Tags.AppendUnique(te)
			}
		}
	} else {
		eev.CreatedAt = nostr.Now()
		eev.Kind = nostr.KindTextNote
	}
	eev.Tags = eev.Tags.AppendUnique(nostr.Tag{"t", "バズワードランキング"})
	eev.Sign(sk)

	success := 0
	for _, r := range relays {
		relay, err := nostr.RelayConnect(context.Background(), r)
		if err != nil {
			continue
		}
		status, err := relay.Publish(context.Background(), eev)
		relay.Close()
		if err == nil && status != nostr.PublishStatusFailed {
			success++
		}
	}
	if success == 0 {
		return errors.New("failed to publish")
	}
	return nil
}

type Word struct {
	Content string
	Time    time.Time
}

type HotItem struct {
	Word  string
	Count int
}

var (
	d     *dict.Dict
	t     *tokenizer.Tokenizer
	mu    sync.Mutex
	words []Word
)

func init() {
	var err error
	d = ipaneologd.Dict()
	t, err = tokenizer.New(d, tokenizer.OmitBosEos())
	if err != nil {
		log.Fatal(err)
	}
}

func isBad(npub string) bool {
	return slices.ContainsFunc(ignores, func(is string) bool {
		if _, s, err := nip19.Decode(is); err == nil {
			return s.(string) == npub
		}
		return false
	})
}

func collect(wg *sync.WaitGroup, ch chan *nostr.Event) {
	defer wg.Done()

	summarizer := time.NewTicker(time.Hour)
	defer summarizer.Stop()
	deleter := time.NewTicker(10 * time.Minute)
	defer deleter.Stop()

	for {
		var ev *nostr.Event
		select {
		case ev = <-ch:
		case <-summarizer.C:
			postRanks(nil)
			continue
		case <-deleter.C:
			now := time.Now()
			mu.Lock()
			words = slices.DeleteFunc(words, func(word Word) bool {
				return word.Time.Sub(now) > time.Hour
			})
			mu.Unlock()
			continue
		}
		if isBad(ev.PubKey) {
			continue
		}
		tokens := t.Tokenize(normalize(ev.Content))
		seen := map[string]struct{}{}
		for _, token := range tokens {
			if _, ok := seen[token.Surface]; ok {
				continue
			}
			seen[token.Surface] = struct{}{}

			cc := token.Features()
			if isIgnore(d, cc) {
				continue
			}
			fmt.Println(token.Surface)

			mu.Lock()
			words = append(words, Word{
				Content: token.Surface,
				Time:    ev.CreatedAt.Time(),
			})
			if len(words) > 1000 {
				words = words[1:]
			}
			mu.Unlock()
		}
	}
}

func postRanks(ev *nostr.Event) {
	hotwords := map[string]*HotItem{}
	mu.Lock()
	for _, word := range words {
		if i, ok := hotwords[word.Content]; ok {
			i.Count += 1
		} else {
			hotwords[word.Content] = &HotItem{
				Word:  word.Content,
				Count: 1,
			}
		}
	}
	mu.Unlock()

	items := []*HotItem{}
	for _, item := range hotwords {
		if item.Count < 3 {
			continue
		}
		items = append(items, item)
	}
	if len(items) < 10 {
		return
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Count > items[j].Count
	})
	if len(items) > 10 {
		items = items[:10]
	}

	var buf bytes.Buffer
	fmt.Fprintln(&buf, "#バズワードランキング\n")
	for i, item := range items {
		fmt.Fprintf(&buf, "%d位: %s (%d)\n", i+1, item.Word, item.Count)
	}
	err := postEvent(os.Getenv("BOT_NSEC"), relays, ev, buf.String())
	if err != nil {
		log.Println(err)
	}
}

func server() {
	relay, err := nostr.RelayConnect(context.Background(), "wss://yabu.me")
	if err != nil {
		log.Println(err)
		return
	}
	defer relay.Close()

	sub, err := relay.Subscribe(context.Background(), []nostr.Filter{{
		Kinds: []int{nostr.KindTextNote, nostr.KindChannelMessage, nostr.KindProfileMetadata},
	}})
	if err != nil {
		log.Println(err)
		return
	}
	defer sub.Close()

	ch := make(chan *nostr.Event, 10)
	defer close(ch)

	var wg sync.WaitGroup

	wg.Add(1)
	go collect(&wg, ch)

	eose := false
loop:
	for {
		ev, ok := <-sub.Events
		if !ok || ev == nil {
			break loop
		}
		select {
		case <-sub.EndOfStoredEvents:
			eose = true
		default:
		}
		json.NewEncoder(os.Stdout).Encode(ev)
		if eose && strings.TrimSpace(ev.Content) == "バズワードランキング" {
			postRanks(ev)
			continue
		}
		ch <- ev
	}
	wg.Wait()
}

func main() {
	var ver bool
	flag.BoolVar(&ver, "version", false, "show version")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	for {
		server()
		time.Sleep(5 * time.Second)
	}
}
