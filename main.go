package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"image/png"
	"log"
	"net/http"
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
	"github.com/mattn/go-nostrbuild"
	nostr "github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/psykhi/wordclouds"
)

const name = "nostr-buzzword"

const version = "0.0.72"

var revision = "HEAD"

var (
	reLink     = regexp.MustCompile(`\b\w+://\S+\b`)
	reTag      = regexp.MustCompile(`(\B#\S+|\bnostr:\S+)`)
	reJapanese = regexp.MustCompile(`[０-９Ａ-Ｚａ-ｚぁ-ゖァ-ヾ一-鶴]`)

	relays = []string{
		"wss://relay-jp.nostr.wirednet.jp",
		"wss://yabu.me",
		"wss://relay.nostr.band",
		"wss://nos.lol",
	}

	ignores  = []string{}
	badWords = []string{
		"ー",
		"〜",
		"is",
		"of",
		"at",
		"in",
		"to",
		"I",
		"me",
		"a",
		"and",
		"/",
		"RE:",
	}
)

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

func normalize(s string) string {
	// remove URLs
	s = reLink.ReplaceAllString(s, "")
	// remove Tags
	s = reTag.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func isIgnoreWord(s string) bool {
	return slices.Contains(badWords, s)
}

func isWhiteSpace(d *dict.Dict, c []string) bool {
	return len(c) == 0 || c[0] == "空白"
}

func isSymbolWord(d *dict.Dict, c []string) bool {
	return len(c) == 0 || c[0] == "記号"
}

func isIgnoreKind(d *dict.Dict, c []string) bool {
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
	defer relay.Close()

	err = relay.Publish(context.Background(), ev)
	if err != nil {
		log.Println(relay.URL, err)
	} else {
		success.Add(1)
	}
}

func postRanks(nsec string, items []*HotItem, relays []string, ev *nostr.Event) error {
	var buf bytes.Buffer
	tags := nostr.Tags{}
	fmt.Fprint(&buf, "#バズワードランキング\n\n")
	for i, item := range items {
		fmt.Fprintf(&buf, "%d位: #%s (%d)\n", i+1, item.Word, item.Count)
		tags = tags.AppendUnique(nostr.Tag{"t", item.Word})
	}

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

	if ev != nil {
		sign := func(ev *nostr.Event) error {
			ev.PubKey = eev.PubKey
			return ev.Sign(sk)
		}
		img, err := makeWordCloud(items, sign)
		if err != nil {
			return err
		}
		fmt.Fprint(&buf, "\n"+img)
	}

	eev.Content = buf.String()
	if ev != nil {
		eev.CreatedAt = ev.CreatedAt + 1
		eev.Kind = ev.Kind
		eev.Tags = tags
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

func isIgnoreNpub(pub string) bool {
	npub, err := nip19.EncodePublicKey(pub)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ignores, func(is string) bool {
		return is == npub
	})
}

func appendWord(word string, t time.Time) {
	if word == "" {
		return
	}
	if isIgnoreWord(word) {
		return
	}

	mu.Lock()
	fmt.Println("===>", word)
	words = append(words, Word{
		Content: word,
		Time:    t,
	})
	if len(words) > 1000 {
		words = words[1:]
	}
	mu.Unlock()
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
				log.Printf("Stoped reading events")
				return
			}
		case <-summarizer.C:
			log.Printf("Run Summarizer")
			if ranks, err := makeRanks(false); err == nil {
				err := postRanks(os.Getenv("BOT_NSEC"), ranks, relays, nil)
				if err != nil {
					log.Println(err)
				}
			}
			continue
		case <-deleter.C:
			log.Printf("Run Deleter")
			now := time.Now()
			mu.Lock()
			words = slices.DeleteFunc(words, func(word Word) bool {
				return word.Time.Sub(now) > time.Hour
			})
			mu.Unlock()
			continue
		}

		collectWords(ev)
	}
}

func removeDuplicate[T any](arr []T, f func(T) string) []T {
	keys := make(map[string]struct{})
	result := []T{}
	for _, item := range arr {
		s := f(item)
		if _, ok := keys[s]; !ok {
			keys[s] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}

func makeRanks(full bool) ([]*HotItem, error) {
	// count the number of appearances per word
	hotwords := map[string]*HotItem{}
	mu.Lock()
	for _, word := range words {
		content := strings.ToLower(word.Content)
		if i, ok := hotwords[content]; ok {
			i.Count++
		} else {
			hotwords[content] = &HotItem{
				Word:  word.Content,
				Count: 1,
			}
		}
	}
	mu.Unlock()

	// make list of items to sort
	items := []*HotItem{}
	for _, item := range hotwords {
		if item.Count < 3 && !full {
			continue
		}
		items = append(items, item)
	}

	items = removeDuplicate(items, func(e *HotItem) string { return e.Word })

	if len(items) < 10 {
		return nil, fmt.Errorf("too less: %v items", len(items))
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Count > items[j].Count
	})
	if !full && len(items) > 10 {
		items = items[:10]
	}
	return items, nil
}

func makeWordCloud(items []*HotItem, sign func(*nostr.Event) error) (string, error) {
	colors := []color.Color{
		color.RGBA{0x1b, 0x1b, 0x1b, 0xff},
		color.RGBA{0x48, 0x48, 0x4B, 0xff},
		color.RGBA{0x59, 0x3a, 0xee, 0xff},
		color.RGBA{0x65, 0xCD, 0xFA, 0xff},
		color.RGBA{0x70, 0xD6, 0xBF, 0xff},
	}

	inputWords := map[string]int{}
	for _, item := range items {
		inputWords[item.Word] = item.Count
	}
	img := wordclouds.NewWordcloud(inputWords,
		wordclouds.FontFile(env("FONTFILE", "Koruri-Regular.ttf")),
		wordclouds.FontMaxSize(100),
		wordclouds.FontMinSize(10),
		wordclouds.Colors(colors),
		wordclouds.Height(800),
		wordclouds.Width(800),
		wordclouds.RandomPlacement(false),
		wordclouds.BackgroundColor(color.RGBA{255, 255, 255, 255}),
		wordclouds.WordSizeFunction("linear"),
	).Draw()

	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	if err != nil {
		return "", err
	}

	result, err := nostrbuild.Upload(&buf, sign)
	if err != nil {
		return "", err
	}
	return result.Data[0].URL, nil
}

func heartbeatPush(url string) {
	resp, err := http.Get(url)
	if err != nil {
		log.Println(err.Error())
		return
	}
	defer resp.Body.Close()
}

func server() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := nostr.NewSimplePool(ctx)
	filters := []nostr.Filter{{
		Kinds: []int{nostr.KindTextNote, nostr.KindChannelMessage},
	}}
	relays := []string{
		"wss://yabu.me",
		"wss://relay-jp.nostr.wirednet.jp",
	}
	sub := pool.SubMany(ctx, relays, filters)
	defer close(sub)

	ch := make(chan *nostr.Event, 10)
	defer close(ch)

	hbtimer := time.NewTicker(5 * time.Minute)
	defer hbtimer.Stop()

	var wg sync.WaitGroup

	wg.Add(1)
	go collect(&wg, ch)

	retry := 0
events_loop:
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				break events_loop
			}
			select {
			case <-ctx.Done():
				log.Printf("connection closed: %v", ctx.Err())
				break events_loop
			default:
			}
			json.NewEncoder(os.Stdout).Encode(ev.Event)
			if strings.TrimSpace(ev.Content) == "バズワードランキング" {
				if ev.CreatedAt.Time().Sub(time.Now()).Seconds() < 10 {
					// post ranking summary as reply
					if ranks, err := makeRanks(true); err == nil {
						err := postRanks(os.Getenv("BOT_NSEC"), ranks, relays, ev.Event)
						if err != nil {
							log.Println(err)
						}
					}
					continue
				}
			}
			// otherwise send the ev to goroutine
			ch <- ev.Event
			retry = 0
		case <-hbtimer.C:
			if url := os.Getenv("HEARTBEAT_URL"); url != "" {
				go heartbeatPush(url)
			}
		case <-time.After(10 * time.Second):
			alive := pool.Relays.Size()
			pool.Relays.Range(func(key string, relay *nostr.Relay) bool {
				if relay.ConnectionError != nil {
					log.Println(relay.ConnectionError, relay.IsConnected())
					alive--
				}
				return true
			})
			if alive == 0 {
				break events_loop
			}
			retry++
			log.Println("Health check", retry)
			if retry > 60 {
				break events_loop
			}
		}
	}
	wg.Wait()
}

func collectWords(ev *nostr.Event) {
	// check ignored npub
	if isIgnoreNpub(ev.PubKey) {
		return
	}
	if strings.ContainsAny(ev.Content, " \t\n") && !reJapanese.MatchString(ev.Content) {
		return
	}
	tokens := t.Tokenize(normalize(ev.Content))
	seen := map[string]struct{}{}
	prev := ""
	prevprev := ""
	for _, token := range tokens {
		cc := token.Features()
		fmt.Println(cc, token.Surface)

		if _, ok := seen[token.Surface]; ok {
			// ignore word seen
			continue
		}
		seen[token.Surface] = struct{}{}

		// check ignored kind of parts
		if isWhiteSpace(d, cc) {
			continue
		}
		if isSymbolWord(d, cc) {
			appendWord(prev, ev.CreatedAt.Time())
			prev = ""
			continue
		}

		if cc[0] == "名詞" {
			if prev == "" && prevprev != "" {
				prev = prevprev
			}
			if cc[1] == "一般" || cc[1] == "固有名詞" || cc[1] == "サ変接続" || cc[1] == "数" {
				if !strings.ContainsAny(token.Surface, "()〜#*/") {
					prev += token.Surface
					continue
				}
			}
			if prev != "" && cc[1] == "接尾" {
				prev += token.Surface
				continue
			}
		} else if cc[0] == "カスタム名詞" {
			if prev == "" && prevprev != "" {
				prev = prevprev
			}
			prev += token.Surface
			continue
		} else if prev != "" && cc[0] == "助詞" && cc[1] == "接尾" {
			prev += token.Surface
			continue
		} else if cc[0] == "形容詞" {
			prevprev = token.Surface
		}

		appendWord(prev, ev.CreatedAt.Time())
		prev = ""
	}
	appendWord(prev, ev.CreatedAt.Time())
	prev = ""
}

func test() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var ev nostr.Event
		err := json.Unmarshal([]byte(scanner.Text()), &ev)
		if err != nil {
			continue
		}

		collectWords(&ev)
	}

	items, err := makeRanks(true)
	if err != nil {
		log.Fatal(err)
	}
	for i, item := range items {
		fmt.Fprintf(os.Stdout, "%d位: %s (%d)\n", i+1, item.Word, item.Count)
	}
}

func env(name string, def string) string {
	if val := os.Getenv(name); val != "" {
		return val
	}
	return def
}

func main() {
	var ver, tt bool
	var ignoresFile string
	var userdicFile string
	flag.BoolVar(&tt, "t", false, "test")
	flag.BoolVar(&ver, "version", false, "show version")
	flag.StringVar(&ignoresFile, "ignores", env("IGNORES", "ignores.txt"), "path to ignores.txt")
	flag.StringVar(&userdicFile, "userdic", env("USERDIC", "userdic.txt"), "path to userdic.txt")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	var err error
	d = ipaneologd.Dict()

	// load userdic.txt
	udict, err := dict.NewUserDict(userdicFile)
	if err == nil {
		t, err = tokenizer.New(d, tokenizer.UserDict(udict), tokenizer.OmitBosEos())
	} else {
		t, err = tokenizer.New(d, tokenizer.OmitBosEos())
	}
	if err != nil {
		log.Fatal(err)
	}

	// load ignores.txt
	f, err := os.Open(ignoresFile)
	if err == nil {
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

	if tt {
		test()
		os.Exit(0)
	}

	for {
		server()
		time.Sleep(5 * time.Second)
	}
}
