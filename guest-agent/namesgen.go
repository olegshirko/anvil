package main

import (
	"context"
	"fmt"
	"math/rand/v2"
)

// Docker names unnamed containers adjective_surname ("eager_hopper"); tools
// and people refer to containers by those names, and `docker ps` showed the
// 64-character ID in the NAMES column instead.

var nameAdjectives = []string{
	"admiring", "agile", "amazing", "awesome", "blissful", "bold", "brave",
	"busy", "calm", "charming", "clever", "cool", "crisp", "curious",
	"dazzling", "determined", "diligent", "dreamy", "eager", "ecstatic",
	"elastic", "elegant", "epic", "fervent", "festive", "focused", "friendly",
	"gallant", "gentle", "gifted", "goofy", "gracious", "happy", "hopeful",
	"humble", "inspiring", "jolly", "jovial", "keen", "kind", "laughing",
	"lucid", "magical", "modest", "musing", "nervous", "nifty", "nimble",
	"nostalgic", "optimistic", "patient", "peaceful", "pedantic", "pensive",
	"practical", "priceless", "quirky", "quizzical", "relaxed", "reverent",
	"romantic", "serene", "sharp", "silly", "sleepy", "stoic", "sweet",
	"tender", "thirsty", "trusting", "upbeat", "vibrant", "vigilant",
	"vigorous", "wizardly", "wonderful", "xenodochial", "youthful", "zealous",
	"zen",
}

var nameSurnames = []string{
	"agnesi", "albattani", "allen", "archimedes", "babbage", "banach",
	"bardeen", "bartik", "bell", "bhabha", "boole", "borg", "bose", "brahmagupta",
	"brattain", "cannon", "carson", "cerf", "chandrasekhar", "clarke", "curie",
	"darwin", "dijkstra", "einstein", "elion", "engelbart", "euclid", "euler",
	"faraday", "fermat", "fermi", "feynman", "franklin", "galileo", "gauss",
	"goldberg", "goodall", "hamilton", "hawking", "heisenberg", "hodgkin",
	"hopper", "hypatia", "jackson", "johnson", "kalam", "kepler", "khorana",
	"knuth", "lamarr", "lamport", "leakey", "liskov", "lovelace", "lumiere",
	"mayer", "mccarthy", "mclean", "meitner", "mendel", "mirzakhani", "morse",
	"napier", "newton", "nobel", "noether", "pascal", "pasteur", "perlman",
	"pike", "poincare", "ramanujan", "ritchie", "rosalind", "sammet", "shannon",
	"sinoussi", "stallman", "swartz", "tesla", "thompson", "torvalds", "turing",
	"varahamihira", "wiles", "wozniak", "wright", "yalow", "yonath",
}

// randomContainerName returns a Docker-style name; after a few collisions
// it appends a digit, as Docker does.
func randomContainerName(retry int) string {
	n := fmt.Sprintf("%s_%s", nameAdjectives[rand.IntN(len(nameAdjectives))],
		nameSurnames[rand.IntN(len(nameSurnames))])
	if retry > 0 {
		n += fmt.Sprint(rand.IntN(10))
	}
	return n
}

// generateContainerName picks a name no container in ns uses yet.
func generateContainerName(ctx context.Context, ns string) string {
	for i := 0; i < 20; i++ {
		name := randomContainerName(i / 5)
		if existing, err := findContainerByName(ctx, ns, name); err == nil && existing == "" {
			return name
		}
	}
	return randomContainerName(1) + fmt.Sprint(rand.IntN(1000))
}
