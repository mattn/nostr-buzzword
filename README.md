# nostr-buzzword

![](https://image.nostr.build/a69e388c4bdf0ea8a60b2380337ad7518247c958047ad85df847b04ae35c30c5.png)

Buzz Word bot on nostr

## Usage

```
$ BOT_NSEC=nsecxxxxxxx nostr-buzzword
```

This bot replies buzz word ranking summary if you post `バズワードランキング` on timeline or group chat.

## Installation

```
go install github.com/mattn/nostr-buzzword@latest
```

Bots listed on https://github.com/nostr-jp/botlist are ignored automatically. The list is fetched at startup and every 6 hours, so a newly registered bot takes effect without a redeploy. Set $BOTLIST_URL to use another list, or set it empty to disable the fetch.

If you would like to ignore some npub(s) in addition to that list, set $IGNORES for the path to the ignores.txt which is listed npub hex.

If you would like to use user dictionary to use customized tokenizer, set $USERDIC for the path to the userdic.txt written as mecab dictionary format.

## License

MIT

Koruri-Regular.ttf is provided from https://koruri.github.io/

## Author

Yasuhiro Matsumoto (a.k.a. mattn)
