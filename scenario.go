// The fixed scenario identity, derived the same way the kit's wire.ts derives it:
// SCENARIO_SEED 0x6a09e667f3bcc909, FNV-1a + splitmix64 finalizer + xorshift64* Rng,
// generateUsers(scenarioRng('users'), ...) — the viewer is users[0].
// Reproducing the generator (rather than hard-coding the login) keeps the entry honest
// if the seed ever changes; the derived values are verified against the kit's own
// buildScenario() in local tests (viewer must be adaiversen / 1000).

package main

import "strconv"

const scenarioSeed uint64 = 0x6a09e667f3bcc909

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// rng is the scenario's xorshift64* generator.
type rng struct {
	state uint64
}

func newRng(seed uint64) *rng {
	masked := seed
	if masked == 0 {
		masked = scenarioSeed
	}
	return &rng{state: masked}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const rangeSize = 4294967296
	limit := uint32(rangeSize - (rangeSize % uint64(maxExclusive)))
	for {
		value := r.nextU32()
		if value < limit {
			return int(value % uint32(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// scenarioUser is one generated identity.
type scenarioUser struct {
	id    int
	login string
	name  string
	email string
}

func generateUsers(r *rng, count int) []scenarioUser {
	used := make(map[string]bool, count)
	users := make([]scenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := ""
		for _, ch := range first + last {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				base += string(ch)
			}
		}
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + strconv.Itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenarioUser{
			id:    1000 + i,
			login: login,
			name:  first + " " + last,
			email: login + "@example.invalid",
		})
	}
	return users
}

var scenarioUsers = generateUsers(newRng(scenarioSeedFor("users")), 3)

// viewer is the token owner's identity: the scenario's viewer (adaiversen, id 1000).
var viewer = scenarioUsers[0]

// teammate is users[1] — not served, kept for reference.
var teammate = scenarioUsers[1]

// stranger is users[2] — repos under this login must 404.
var stranger = scenarioUsers[2]
