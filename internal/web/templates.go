package web

import (
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"kaepora/internal/back"
	"kaepora/internal/util"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/russross/blackfriday/v2"
)

func (s *Server) loadTemplates(baseDir string) (map[string]*template.Template, error) {
	layouts, err := filepath.Glob(filepath.Join(baseDir, "templates/layouts/*.html"))
	if err != nil {
		return nil, err
	}

	includes, err := filepath.Glob(filepath.Join(baseDir, "templates/includes/*.html"))
	if err != nil {
		return nil, err
	}

	ret := make(map[string]*template.Template, len(layouts))
	for _, layout := range layouts {
		tpl, err := template.New("").
			Funcs(s.getTemplateFuncMap(baseDir)).
			ParseFiles(append(includes, layout)...)
		if err != nil {
			return nil, err
		}

		key := strings.TrimPrefix(layout, filepath.Join(baseDir, "templates/layouts")+"/")
		ret[key] = tpl
	}

	return ret, nil
}

func (s *Server) getTemplateFuncMap(baseDir string) template.FuncMap {
	return template.FuncMap{
		"t": func(locale string, str string) string {
			return s.locales[locale].Get(str)
		},

		"tf": func(locale string, str string, args ...interface{}) string {
			return fmt.Sprintf(s.locales[locale].Get(str), args...)
		},

		"tmd": func(locale, str string) template.HTML {
			return template.HTML(blackfriday.Run( // nolint:gosec
				[]byte(s.locales[locale].Get(str)),
			))
		},

		"matchSessionStatusTag": func(locale string, status back.MatchSessionStatus) template.HTML {
			var str, class string
			switch status {
			case back.MatchSessionStatusWaiting:
				str = s.locales[locale].Get("planned")
				class = "is-success is-light"
			case back.MatchSessionStatusJoinable:
				str = s.locales[locale].Get("joinable")
				class = "is-success is-light"
			case back.MatchSessionStatusPreparing:
				str = s.locales[locale].Get("preparing")
				class = "is-warning is-light"
			case back.MatchSessionStatusInProgress:
				str = s.locales[locale].Get("in progress")
				class = "is-success"
			case back.MatchSessionStatusClosed:
				str = s.locales[locale].Get("closed")
			default:
				return ""
			}

			return template.HTML(fmt.Sprintf(`<span class="tag is-medium is-rounded %s">%s</span>`, class, str)) // nolint:gosec
		},

		"ranking":        tplRanking,
		"until":          tplUntil,
		"future":         tplFuture,
		"datetime":       util.Datetime,
		"assetURL":       tplAssetURL,
		"assetIntegrity": tplAssetIntegrity(baseDir),
	}
}

func tplRanking(v back.LeaderboardEntry) string {
	// Per Glicko-2, the rating is the interval R±2×RD where we expect the
	// actual skill level to be with 95% confidence.
	// This is rounded down to 50 increments for clarity.
	d := 2.0 * v.Deviation
	min := int(math.Round((v.Rating-d)/50.0) * 50.0)
	max := int(math.Round((v.Rating+d)/50.0) * 50.0)
	return fmt.Sprintf("%d-%d", min, max)
}

func tplUntil(iface interface{}, trunc string) string {
	var t time.Time
	switch iface := iface.(type) {
	case time.Time:
		t = iface
	case util.TimeAsDateTimeTZ:
		t = iface.Time()
	default:
		panic(fmt.Errorf("unexpected type %T", iface))
	}

	delta := time.Until(t)

	switch trunc {
	case "m":
		delta = delta.Truncate(time.Minute)
		return strings.TrimSuffix(delta.String(), "0s")
	default: // nolint: gocritic,stylecheck
		fallthrough
	case "s":
		return delta.Truncate(time.Second).String()
	}
}

func tplFuture(iface interface{}) bool {
	var t time.Time
	switch iface := iface.(type) {
	case time.Time:
		t = iface
	case util.TimeAsDateTimeTZ:
		t = iface.Time()
	default:
		panic(fmt.Errorf("unexpected type %T", iface))
	}

	return t.After(time.Now())
}

func tplAssetURL(name string) string {
	return "/_/" + name
}

func tplAssetIntegrity(baseDir string) func(name string) (string, error) {
	hashCache := map[string]string{}

	return func(name string) (string, error) {
		if hash, ok := hashCache[name]; ok {
			return hash, nil
		}

		f, err := os.Open(filepath.Join(baseDir, "static", name))
		if err != nil {
			return "", err
		}
		defer f.Close()

		h := sha512.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}

		hashCache[name] = "sha512-" + base64.StdEncoding.EncodeToString(h.Sum(nil))
		return hashCache[name], nil
	}
}
