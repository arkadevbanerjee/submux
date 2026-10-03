package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// kiroPathPrefix is the profile marker: a Claude Code profile whose
// ANTHROPIC_BASE_URL is http://127.0.0.1:8787/kiro reaches submux with
// /kiro/v1/messages. submux strips the prefix and runs the request in Kiro
// mode, where no id can ever fall through to the claude-* Anthropic route.
const kiroPathPrefix = "/kiro"

const kiroIDPrefix = "kiro/"

// kiroModelRe parses claude-<family>-<major>[.-<minor>][-YYYYMMDD]; a bare
// major means minor 0 (kiro/claude-opus-5).
var kiroModelRe = regexp.MustCompile(`^claude-([a-z]+)-(\d+)(?:[.-](\d{1,2}))?(?:-\d{8})?$`)

type kiroModel struct {
	id       string
	family   string
	maj, min int
	oneM     bool
}

func parseKiroModel(id string) (kiroModel, bool) {
	m := kiroModel{id: id}
	base := strings.TrimPrefix(id, kiroIDPrefix)
	if strings.HasSuffix(base, "[1m]") {
		m.oneM = true
		base = strings.TrimSuffix(base, "[1m]")
	}
	g := kiroModelRe.FindStringSubmatch(base)
	if g == nil {
		return m, false
	}
	m.family = g[1]
	m.maj, _ = strconv.Atoi(g[2])
	if g[3] != "" {
		m.min, _ = strconv.Atoi(g[3])
	}
	return m, true
}

// newer reports whether a is a newer version than b.
func (a kiroModel) newer(b kiroModel) bool {
	if a.maj != b.maj {
		return a.maj > b.maj
	}
	return a.min > b.min
}

// kiroResolve maps a requested model id onto an id from the live Kiro list
// (ids carry the "kiro/" prefix). It tries, in order: an exact live id;
// the same family and version in any spelling (dash/dot, dated, [1m] kept
// when Kiro has it); then the newest model of the same family (fable has no
// Kiro equivalent and uses opus). On failure ok is false and closest lists
// live ids to suggest.
func kiroResolve(requested string, live []string) (sent string, closest []string, ok bool) {
	want := strings.TrimPrefix(requested, kiroIDPrefix)
	for _, id := range live {
		if id == kiroIDPrefix+want {
			return id, nil, true
		}
	}

	var models []kiroModel
	for _, id := range live {
		if m, ok := parseKiroModel(id); ok {
			models = append(models, m)
		}
	}
	// Newest first, then [1m]-less, dot spelling, and id order for stability.
	sort.Slice(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if a.maj != b.maj || a.min != b.min {
			return a.newer(b)
		}
		return a.id < b.id
	})

	req, parsed := parseKiroModel(want)
	if !parsed {
		return "", suggestKiro(models, ""), false
	}
	family := req.family
	exact := family != "fable"
	if family == "fable" {
		family = "opus"
	}
	var fam []kiroModel
	for _, m := range models {
		if m.family == family {
			fam = append(fam, m)
		}
	}
	if len(fam) == 0 {
		return "", suggestKiro(models, ""), false
	}
	version := fam[0] // newest
	if exact {
		for _, m := range fam {
			if m.maj == req.maj && m.min == req.min {
				version = m
				break
			}
		}
	}
	var best *kiroModel
	for i, m := range fam {
		if m.maj != version.maj || m.min != version.min {
			continue
		}
		if best == nil || kiroBetter(m, *best, req.oneM) {
			best = &fam[i]
		}
	}
	return best.id, nil, true
}

// kiroBetter ranks two spellings of the same version: the wanted [1m]
// variant first, then the dot spelling (what Kiro itself lists for plain ids).
func kiroBetter(a, b kiroModel, wantOneM bool) bool {
	if (a.oneM == wantOneM) != (b.oneM == wantOneM) {
		return a.oneM == wantOneM
	}
	ad, bd := strings.Contains(a.id, "."), strings.Contains(b.id, ".")
	if ad != bd {
		return ad
	}
	return a.id < b.id
}

// suggestKiro lists up to five live ids, newest first, for an error message.
func suggestKiro(models []kiroModel, family string) []string {
	var out []string
	for _, m := range models {
		if family == "" || m.family == family {
			out = append(out, m.id)
		}
		if len(out) == 5 {
			break
		}
	}
	return out
}

// kiroLiveList caches the live Kiro id list (the kiro/* route's /v1/models)
// for modelsCacheFreshFor, serving a stale list when a refresh fails.
type kiroLiveList struct {
	at  time.Time
	ids []string
}

func (s *server) kiroLive() []string {
	s.kiroMu.Lock()
	defer s.kiroMu.Unlock()
	if s.kiroList.ids != nil && time.Since(s.kiroList.at) < modelsCacheFreshFor {
		return s.kiroList.ids
	}
	rt, ok := matchRoute(s.conf().Routes, kiroIDPrefix+"x")
	if !ok {
		return s.kiroList.ids
	}
	models, err := fetchUpstreamModels(rt)
	if err != nil {
		return s.kiroList.ids
	}
	ids := []string{}
	for _, m := range models {
		if strings.HasPrefix(m.ID, kiroIDPrefix) && isPickableModelID(m.ID) {
			ids = append(ids, m.ID)
		}
	}
	s.kiroList = kiroLiveList{at: time.Now(), ids: ids}
	return ids
}
