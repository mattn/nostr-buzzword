package main

import (
	"image"
	"image/color"
	"math"
	"math/rand"
	"sort"
	"strings"

	"github.com/fogleman/gg"
	"golang.org/x/image/font"
)

// wcWord is a single word to be placed on the cloud.
type wcWord struct {
	word string
	size float64
}

// wcBox is an axis-aligned bounding box used for collision detection.
type wcBox struct {
	left, top, right, bottom float64
}

func (b wcBox) overlaps(o wcBox) bool {
	return b.left < o.right && b.right > o.left &&
		b.top < o.bottom && b.bottom > o.top
}

func (b wcBox) fits(w, h float64) bool {
	return b.left >= 0 && b.top >= 0 && b.right <= w && b.bottom <= h
}

// wordCloudConfig configures the self-implemented word cloud generator.
type wordCloudConfig struct {
	fontFile      string
	width         int
	height        int
	fontMaxSize   float64
	fontMinSize   float64
	colors        []color.Color
	background    color.Color
	verticalRatio float64 // probability [0,1] that a word is drawn rotated 90 degrees
}

// drawWordCloud renders inputWords into an image, mixing horizontal and
// vertical (90-degree rotated) words. It replaces github.com/psykhi/wordclouds
// so that orientation can be controlled per word.
func drawWordCloud(inputWords map[string]int, cfg wordCloudConfig) (image.Image, error) {
	words := make([]wcWord, 0, len(inputWords))
	maxCount := 0
	for _, c := range inputWords {
		if c > maxCount {
			maxCount = c
		}
	}
	if maxCount == 0 {
		maxCount = 1
	}
	for w, c := range inputWords {
		size := (float64(c) / float64(maxCount)) * cfg.fontMaxSize
		if size < cfg.fontMinSize {
			size = cfg.fontMinSize
		}
		words = append(words, wcWord{word: strings.TrimSpace(w), size: size})
	}
	// Largest words first so they grab the prime spots near the center.
	sort.Slice(words, func(i, j int) bool {
		return words[i].size > words[j].size
	})

	dc := gg.NewContext(cfg.width, cfg.height)
	dc.SetColor(cfg.background)
	dc.Clear()

	fonts := map[float64]font.Face{}
	setFont := func(size float64) error {
		face, ok := fonts[size]
		if !ok {
			var err error
			face, err = gg.LoadFontFace(cfg.fontFile, size)
			if err != nil {
				return err
			}
			fonts[size] = face
		}
		dc.SetFontFace(face)
		return nil
	}

	w := float64(cfg.width)
	h := float64(cfg.height)
	cx := w / 2
	cy := h / 2
	maxRadius := math.Sqrt(w*w+h*h) / 2

	placed := make([]wcBox, 0, len(words))
	consecutiveMisses := 0

	for _, word := range words {
		if word.word == "" {
			continue
		}
		if err := setFont(word.size); err != nil {
			return nil, err
		}

		tw, th := dc.MeasureString(word.word)
		// The first word that actually lands sits at the dead center; keep it
		// horizontal so the centerpiece is always upright.
		vertical := len(placed) > 0 && rand.Float64() < cfg.verticalRatio

		// Bounding box dimensions on the canvas (swapped when rotated).
		bw, bh := tw+6, th+6
		if vertical {
			bw, bh = th+6, tw+6
		}

		x, y, ok := findSpot(placed, bw, bh, cx, cy, w, h, maxRadius)
		if !ok {
			consecutiveMisses++
			if consecutiveMisses > 10 {
				break
			}
			continue
		}
		consecutiveMisses = 0

		dc.SetColor(cfg.colors[rand.Intn(len(cfg.colors))])
		if vertical {
			// Randomly tilt vertical words either way (90 or 270 degrees).
			angle := 90.0
			if rand.Intn(2) == 0 {
				angle = 270.0
			}
			dc.Push()
			dc.RotateAbout(gg.Radians(angle), x, y)
			dc.DrawStringAnchored(word.word, x, y, 0.5, 0.5)
			dc.Pop()
		} else {
			dc.DrawStringAnchored(word.word, x, y, 0.5, 0.5)
		}

		placed = append(placed, wcBox{
			left:   x - bw/2,
			top:    y - bh/2,
			right:  x + bw/2,
			bottom: y + bh/2,
		})
	}

	return dc.Image(), nil
}

// findSpot walks an Archimedean-style spiral of circles outward from the center
// and returns the first center point where a bw x bh box neither leaves the
// canvas nor overlaps an already placed word.
func findSpot(placed []wcBox, bw, bh, cx, cy, w, h, maxRadius float64) (float64, float64, bool) {
	for r := 0.0; r <= maxRadius; r += 5.0 {
		// Keep the angular sampling density roughly constant as r grows.
		steps := int(math.Max(8, 2*math.Pi*r/5))
		for i := 0; i < steps; i++ {
			theta := 2 * math.Pi * float64(i) / float64(steps)
			x := cx + r*math.Cos(theta)
			y := cy + r*math.Sin(theta)

			box := wcBox{
				left:   x - bw/2,
				top:    y - bh/2,
				right:  x + bw/2,
				bottom: y + bh/2,
			}
			if !box.fits(w, h) {
				continue
			}
			collides := false
			for _, p := range placed {
				if box.overlaps(p) {
					collides = true
					break
				}
			}
			if !collides {
				return x, y, true
			}
		}
		if r == 0 {
			// r==0 yields a single point; advance the spiral.
			continue
		}
	}
	return 0, 0, false
}
