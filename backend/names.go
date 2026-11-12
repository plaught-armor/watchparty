package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

// nameGenerator builds room and user names from word lists.
type nameGenerator struct {
	adjectives []string
	nouns      []string
	verbs      []string
}

func loadWords(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	words := strings.Fields(string(raw))
	if len(words) == 0 {
		return nil, fmt.Errorf("%s: no words", path)
	}
	return words, nil
}

func newNameGenerator(dir string) (*nameGenerator, error) {
	var lists [3][]string
	for i, file := range [3]string{"adjectives.txt", "nouns.txt", "verbs.txt"} {
		words, err := loadWords(filepath.Join(dir, file))
		if err != nil {
			return nil, err
		}
		lists[i] = words
	}
	return &nameGenerator{adjectives: lists[0], nouns: lists[1], verbs: lists[2]}, nil
}

func pick(words []string) string {
	return words[rand.IntN(len(words))]
}

func capitalize(word string) string {
	return strings.ToUpper(word[:1]) + word[1:]
}

// roomName returns an ID like "happy-otter-jump".
func (g *nameGenerator) roomName() string {
	return pick(g.adjectives) + "-" + pick(g.nouns) + "-" + pick(g.verbs)
}

// userName returns a display name like "Happy Otter".
func (g *nameGenerator) userName() string {
	return capitalize(pick(g.adjectives)) + " " + capitalize(pick(g.nouns))
}
