package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikawaha/kagome-dict-ipa-neologd"
	"github.com/ikawaha/kagome-dict/dict"
	"github.com/ikawaha/kagome/v2/tokenizer"
	nostr "github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const name = "nostr-buzzword"

const version = "0.0.24"

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

	ignores = []string{}
)

func normalize(s string) string {
	// remove URLs
	s = reLink.ReplaceAllString(s, "")
	// remove Tags
	s = reTag.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func isIgnore(d *dict.Dict, c []string) bool {
	if len(c) == 0 {
		return true
	}
	if c[0] != "名詞" && c[0] != "副詞" && c[0] != "カスタム名詞" {
		return true
	}
	if c[0] == "名詞" && c[1] != "一般" && c[1] != "固有名詞" {
		return true
	}
	return false
}

func publishEvent(wg *sync.WaitGroup, r string, ev nostr.Event, success *atomic.Int64) {
	defer wg.Done()

	relay, err := nostr.RelayConnect(context.Background(), r)
	if err != nil {
		log.Println(relay.URL, err)
		return
	}
	status, err := relay.Publish(context.Background(), ev)
	if err != nil {
		log.Println(relay.URL, status, err)
	}
	relay.Close()
	if err == nil && status != nostr.PublishStatusFailed {
		success.Add(1)
	}
}

func postEvent(nsec string, relays []string, ev *nostr.Event, content string) error {
	eev := nostr.Event{}
	var sk string
	if _, s, err := nip19.Decode(nsec); err == nil {
		sk = s.(string)
	} else {
		return err
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
		eev.Tags = eev.Tags.AppendUnique(nostr.Tag{"p", ev.PubKey})
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

	var success atomic.Int64
	var wg sync.WaitGroup
	for _, r := range relays {
		wg.Add(1)
		go publishEvent(&wg, r, eev, &success)
	}
	wg.Wait()
	if success.Load() == 0 {
		return errors.New("failed to publish")
	}
	return nil
}

// Word is structure of word
type Word struct {
	Content string
	Time    time.Time
}

// HotItem is structure of hot item
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

	// load userdic.txt
	if dic := os.Getenv("USERDIC"); dic != "" {
		udict, err := dict.NewUserDict(dic)
		if err != nil {
			log.Fatal(err)
		}
		t, err = tokenizer.New(d, tokenizer.UserDict(udict), tokenizer.OmitBosEos())
	} else {
		t, err = tokenizer.New(d, tokenizer.OmitBosEos())
	}
	if err != nil {
		log.Fatal(err)
	}

	// load ignores.txt
	if ign := os.Getenv("IGNORES"); ign != "" {
		f, err := os.Open(ign)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasPrefix(text, "#") {
				continue
			}
			tok := strings.Split(text, " ")
			if len(tok) >= 1 {
				ignores = append(ignores, tok[0])
			}
		}
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

	// summarizer post a summary every hour
	summarizer := time.NewTicker(time.Hour)
	defer summarizer.Stop()
	// deleter delete old enties
	deleter := time.NewTicker(10 * time.Minute)
	defer deleter.Stop()

	for {
		var ev *nostr.Event
		select {
		case ev = <-ch:
			if ev == nil {
				return
			}
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
		// check ignored npub
		if isBad(ev.PubKey) {
			continue
		}
		tokens := t.Tokenize(normalize(ev.Content))
		seen := map[string]struct{}{}
		for _, token := range tokens {
			if _, ok := seen[token.Surface]; ok {
				// ignore word seen
				continue
			}
			seen[token.Surface] = struct{}{}

			cc := token.Features()
			// check ignored kind of parts
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
	// count the number of appearances per word
	hotwords := map[string]*HotItem{}
	mu.Lock()
	for _, word := range words {
		if i, ok := hotwords[word.Content]; ok {
			i.Count++
		} else {
			hotwords[word.Content] = &HotItem{
				Word:  word.Content,
				Count: 1,
			}
		}
	}
	mu.Unlock()

	// make list of items to sort
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
	fmt.Fprint(&buf, "#バズワードランキング\n\n")
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
		Kinds: []int{nostr.KindTextNote, nostr.KindChannelMessage},
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
		case <-relay.Context().Done():
			log.Printf("connection closed: %v", relay.Context().Err())
			break loop
		default:
		}
		json.NewEncoder(os.Stdout).Encode(ev)
		if eose && strings.TrimSpace(ev.Content) == "バズワードランキング" {
			// post ranking summary as reply
			postRanks(ev)
			continue
		}
		// otherwise send the ev to goroutine
		ch <- ev
	}
	wg.Wait()
}

func test() {
	b, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		log.Fatal(err)
	}
	tokens := t.Tokenize(normalize(string(b)))
	seen := map[string]struct{}{}
	for _, token := range tokens {
		if _, ok := seen[token.Surface]; ok {
			// ignore word seen
			continue
		}
		seen[token.Surface] = struct{}{}

		cc := token.Features()
		fmt.Println(cc)
		if isIgnore(d, cc) {
			continue
		}
		fmt.Println(token.Surface)
	}
}

func main() {
	var ver, t bool
	flag.BoolVar(&t, "t", false, "test")
	flag.BoolVar(&ver, "version", false, "show version")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	if t {
		test()
		os.Exit(0)
	}

	for {
		server()
		time.Sleep(5 * time.Second)
	}
}
