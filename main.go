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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ikawaha/kagome-dict-ipa-neologd"
	"github.com/ikawaha/kagome-dict/dict"
	"github.com/ikawaha/kagome/v2/tokenizer"
	"github.com/mattn/go-nostrbuild"
	nostr "github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip05"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const name = "nostr-buzzword"

const version = "0.0.126"

var revision = "HEAD"

var (
	reLink     = regexp.MustCompile(`\b\w+://\S+\b`)
	reTag      = regexp.MustCompile(`(\B#\S+|\bnostr:\S+)`)
	reBech32   = regexp.MustCompile(`\b(?:npub|nsec|note|nprofile|nevent|naddr|nrelay)1[02-9ac-hj-np-z]{20,}\b`)
	reWordChar = regexp.MustCompile(`[\p{L}\p{N}]`)
	reLetter   = regexp.MustCompile(`\p{L}`)
	reJapanese = regexp.MustCompile(`[０-９Ａ-Ｚａ-ｚぁ-ゖァ-ヾ一-鶴]`)

	relays = []string{
		"wss://relay-jp.nostr.wirednet.jp",
		"wss://yabu.me",
		//"wss://relay.nostr.band",
		"wss://nos.lol",
		"wss://nostr.compile-error.net",
	}

	ignores  = []string{}
	badwords = []string{
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
	Where   string
	PubKey  string
}

// HotItem is structure of hot item
type HotItem struct {
	Word    string
	Count   int
	Authors int
}

// minRankCount and minRankAuthors are the thresholds a word must clear to be
// ranked. Requiring several distinct authors keeps a single account repeating
// the same phrase from taking the top slots on its own.
const (
	minRankCount   = 3
	minRankAuthors = 3
)

func (i *HotItem) rankable() bool {
	return i.Count >= minRankCount && i.Authors >= minRankAuthors
}

var (
	d     *dict.Dict
	t     *tokenizer.Tokenizer
	mu    sync.Mutex
	words []Word
)

var (
	nip05Pool     *nostr.SimplePool
	nip05PoolOnce sync.Once
	skipNip05     bool
)

func normalize(s string) string {
	// remove URLs
	s = reLink.ReplaceAllString(s, "")
	// remove Tags
	s = reTag.ReplaceAllString(s, "")
	// remove bare NIP-19 bech32 entities (e.g. npub1..., note1...)
	s = reBech32.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func isIgnoreWord(s string) bool {
	return slices.Contains(badwords, s)
}

// isNoiseWord reports whether an assembled word is never worth ranking, no
// matter how often it appears.
func isNoiseWord(s string) bool {
	// Digits only, e.g. "1", "00", "5", "2026". The IPA dictionary classifies
	// them as 名詞 数 and they join with following nouns ("5時"), but on their
	// own they carry no meaning.
	if !reLetter.MatchString(s) {
		return true
	}
	// A lone non-ASCII letter is almost always a leftover of a kaomoji such as
	// (´・ω・｀). Single ASCII letters ("X") and single kanji stay.
	if r := []rune(s); len(r) == 1 && r[0] > unicode.MaxASCII && !unicode.Is(unicode.Han, r[0]) {
		return true
	}
	return false
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
	rank := 0
	for _, item := range items {
		if !item.rankable() {
			continue
		}
		rank++
		fmt.Fprintf(&buf, "%d位: #%s (%d)\n", rank, item.Word, item.Count)
		tags = tags.AppendUnique(nostr.Tag{"t", item.Word})
		if rank >= 10 {
			break
		}
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
		eev.Tags = eev.Tags.AppendUnique(nostr.Tag{"e", ev.ID, "", "root"})
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

func getNip05Pool() *nostr.SimplePool {
	nip05PoolOnce.Do(func() {
		nip05Pool = nostr.NewSimplePool(context.Background())
	})
	return nip05Pool
}

// verifyNip05Authors fetches kind:0 metadata for all given pubkeys in a single
// batch and returns the set of pubkeys whose NIP-05 identifier resolves back
// to the same pubkey.
func verifyNip05Authors(ctx context.Context, pubkeys []string) map[string]bool {
	verified := map[string]bool{}
	if skipNip05 {
		for _, p := range pubkeys {
			verified[p] = true
		}
		return verified
	}
	if len(pubkeys) == 0 {
		return verified
	}

	metaCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	metaMap := getNip05Pool().FetchManyReplaceable(metaCtx, relays, nostr.Filter{
		Kinds:   []int{nostr.KindProfileMetadata},
		Authors: pubkeys,
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	metaMap.Range(func(key nostr.ReplaceableKey, ev *nostr.Event) bool {
		pubkey := key.PubKey
		var profile struct {
			Nip05 string `json:"nip05"`
		}
		if err := json.Unmarshal([]byte(ev.Content), &profile); err != nil {
			return true
		}
		if profile.Nip05 == "" {
			return true
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			qCtx, qCancel := context.WithTimeout(ctx, 5*time.Second)
			defer qCancel()
			pp, err := nip05.QueryIdentifier(qCtx, profile.Nip05)
			if err != nil || pp == nil {
				return
			}
			if pp.PublicKey == pubkey {
				mu.Lock()
				verified[pubkey] = true
				mu.Unlock()
			}
		}()
		return true
	})
	wg.Wait()
	return verified
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

func appendWord(where, pubkey, word string, t time.Time) {
	if word == "" {
		return
	}
	if isIgnoreWord(word) {
		return
	}
	if isNoiseWord(word) {
		return
	}

	mu.Lock()
	words = append(words, Word{
		Content: word,
		Time:    t,
		Where:   where,
		PubKey:  pubkey,
	})
	if max := targetWords(); len(words) > max {
		words = words[len(words)-max:]
	}
	mu.Unlock()
}

// targetWords is the number of recent words we keep in the rolling buffer used
// to compute rankings. Verifying authors by NIP-05 drops the words of
// unverified authors at ranking time, so the buffer needs to hold more raw
// words than before to keep enough verified ones. Override with BUZZWORD_WORDS.
func targetWords() int {
	if n, err := strconv.Atoi(os.Getenv("BUZZWORD_WORDS")); err == nil && n > 0 {
		return n
	}
	return 3000
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
			ranks, err := makeRanks("")
			if err != nil {
				log.Println("makeRanks:", err)
			} else if err := postRanks(os.Getenv("BOT_NSEC"), ranks, relays, nil); err != nil {
				log.Println("postRanks:", err)
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

func findWhere(ev *nostr.Event) string {
	for _, tag := range ev.Tags {
		if len(tag) == 4 && tag[0] == "e" && tag[3] == "root" {
			return tag[1]
		}
	}
	return ""
}

func makeRanks(where string) ([]*HotItem, error) {
	// snapshot the words for `where` together with the set of distinct authors
	mu.Lock()
	filtered := make([]Word, 0, len(words))
	authors := map[string]struct{}{}
	for _, word := range words {
		if word.Where != where {
			continue
		}
		filtered = append(filtered, word)
		authors[word.PubKey] = struct{}{}
	}
	mu.Unlock()

	pubkeys := make([]string, 0, len(authors))
	for k := range authors {
		pubkeys = append(pubkeys, k)
	}
	verified := verifyNip05Authors(context.Background(), pubkeys)

	// count the number of appearances per word from verified authors only,
	// together with how many distinct authors used it
	hotwords := map[string]*HotItem{}
	byWord := map[string]map[string]struct{}{}
	kept := 0
	for _, word := range filtered {
		if !verified[word.PubKey] {
			continue
		}
		kept++
		content := strings.ToLower(word.Content)
		i, ok := hotwords[content]
		if !ok {
			i = &HotItem{Word: word.Content}
			hotwords[content] = i
			byWord[content] = map[string]struct{}{}
		}
		i.Count++
		if _, ok := byWord[content][word.PubKey]; !ok {
			byWord[content][word.PubKey] = struct{}{}
			i.Authors++
		}
	}
	log.Printf("makeRanks where=%q words=%d authors=%d verified=%d kept=%d distinct=%d",
		where, len(filtered), len(pubkeys), len(verified), kept, len(hotwords))

	// make list of items to sort (include all words; ranking filter is applied by the caller)
	items := []*HotItem{}
	ranked := 0
	for _, item := range hotwords {
		items = append(items, item)
		if item.rankable() {
			ranked++
		}
	}

	items = removeDuplicate(items, func(e *HotItem) string { return e.Word })

	if ranked < 5 {
		return nil, fmt.Errorf("too less: %v items", ranked)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Count > items[j].Count
	})
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

	verticalRatio := 0.3
	if v := env("BUZZWORD_VERTICAL_RATIO", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			verticalRatio = f
		}
	}

	img, err := drawWordCloud(inputWords, wordCloudConfig{
		fontFile:      env("FONTFILE", "Koruri-Regular.ttf"),
		width:         500,
		height:        500,
		fontMaxSize:   100,
		fontMinSize:   10,
		colors:        colors,
		background:    color.RGBA{255, 255, 255, 255},
		verticalRatio: verticalRatio,
	})
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	err = png.Encode(&buf, img)
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

	hctimer := time.NewTicker(10 * time.Second)
	defer hctimer.Stop()

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

					ranks, err := makeRanks(findWhere(ev.Event))
					if err != nil {
						log.Println("makeRanks:", err)
					} else if err := postRanks(os.Getenv("BOT_NSEC"), ranks, relays, ev.Event); err != nil {
						log.Println("postRanks:", err)
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
		case <-hctimer.C:
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

func join(lhs, rhs string) string {
	lhre := regexp.MustCompile(`\w$`).MatchString(lhs)
	rhre := regexp.MustCompile(`^\w`).MatchString(rhs)
	if lhre && rhre {
		return lhs + " " + rhs
	}
	return lhs + rhs
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
	where := findWhere(ev)

	for _, token := range tokens {
		cc := token.Features()

		if _, ok := seen[token.Surface]; ok {
			// ignore word seen
			continue
		}
		seen[token.Surface] = struct{}{}

		// check ignored kind of parts
		if isWhiteSpace(d, cc) {
			continue
		}
		// Treat as a symbol any token that has no letters/digits. The IPA
		// dictionary classifies operators like "+" and "-" as 名詞 サ変接続,
		// so without this they would slip through and be ranked.
		if isSymbolWord(d, cc) || !reWordChar.MatchString(token.Surface) {
			appendWord(where, ev.PubKey, prev, ev.CreatedAt.Time())
			prev = ""
			prevprev = ""
			continue
		}

		if cc[0] == "名詞" {
			if cc[1] == "一般" || cc[1] == "固有名詞" || cc[1] == "サ変接続" || cc[1] == "数" {
				if !strings.ContainsAny(token.Surface, "()〜#*/") {
					if prev == "" {
						prev = prevprev
					}
					prevprev = ""
					prev = join(prev, token.Surface)
					continue
				}
			}
			if prev != "" && cc[1] == "接尾" {
				prev = join(prev, token.Surface)
				continue
			}
		} else if cc[0] == "カスタム名詞" {
			if prev == "" {
				prev = prevprev
			}
			prevprev = ""
			prev = join(prev, token.Surface)
			continue
		} else if prev != "" && cc[0] == "助詞" && cc[1] == "接尾" {
			prev = join(prev, token.Surface)
			continue
		} else if cc[0] == "形容詞" {
			if cc[1] == "自立" {
				// hold the adjective only as a prefix candidate for the
				// noun that immediately follows; never append it alone
				appendWord(where, ev.PubKey, prev, ev.CreatedAt.Time())
				prev = ""
				prevprev = token.Surface
				continue
			}
			if prev != "" && cc[1] == "接尾" {
				// e.g. 子供っぽい
				prev = join(prev, token.Surface)
				continue
			}
		}

		appendWord(where, ev.PubKey, prev, ev.CreatedAt.Time())
		prev = ""
		prevprev = ""
	}
	appendWord(where, ev.PubKey, prev, ev.CreatedAt.Time())
}

func test() {
	skipNip05 = true
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var ev nostr.Event
		err := json.Unmarshal([]byte(scanner.Text()), &ev)
		if err != nil {
			continue
		}

		collectWords(&ev)
	}

	items, err := makeRanks("")
	if err != nil {
		log.Fatal(err)
	}
	rank := 0
	for _, item := range items {
		if !item.rankable() {
			continue
		}
		rank++
		fmt.Fprintf(os.Stdout, "%d位: %s (%d)\n", rank, item.Word, item.Count)
	}
}

func env(name string, def string) string {
	if val := os.Getenv(name); val != "" {
		return val
	}
	return def
}

func init() {
	time.Local = time.FixedZone("Local", 9*60*60)
}

func main() {
	var ver, tt bool
	var ignoresFile string
	var userdicFile string
	var badwordsFile string
	flag.BoolVar(&tt, "t", false, "test")
	flag.BoolVar(&ver, "version", false, "show version")
	flag.StringVar(&ignoresFile, "ignores", env("IGNORES", "ignores.txt"), "path to ignores.txt")
	flag.StringVar(&badwordsFile, "badwords", env("BADWORDS", "badwords.txt"), "path to badwords.txt")
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
		// a broken userdic.txt used to be swallowed here, leaving the whole
		// user dictionary silently unused
		log.Printf("cannot load %s: %v", userdicFile, err)
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

	// load badwords.txt
	f, err = os.Open(badwordsFile)
	if err == nil {
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			text := scanner.Text()
			badwords = append(badwords, text)
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
