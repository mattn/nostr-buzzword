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

If you would like to ignore some npub(s), set $IGNORES for the path to the ignores.txt which is listed npub hex.

If you would like to use user dictionary to use customized tokenizer, set $USERDIC for the path to the userdic.txt written as mecab dictionary format.

## License

MIT

## Author

Yasuhiro Matsumoto (a.k.a. mattn)
