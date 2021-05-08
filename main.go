package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/dustin/go-wikiparse"
	"golang.org/x/time/rate"
)

var (
	flagCountry  = flag.String("country", "", "individual country to run")
	flagPosition = flag.Int("position", 0, "position in list of countries")
)

var (
	// {{Flagicon|Country}} [[Actual Country|Country]]
	reCountry = regexp.MustCompile(`{{Flagicon\|[\w ]+}} \[\[(.+?)[\||\]\]]`)

	// image_map = Country.svg\n
	reImageMap  = regexp.MustCompile(`image_map\s+= (.+?)\n`)
	reImageMap2 = regexp.MustCompile(`image_map2\s+= (.+?)\n`)

	// image_flag = Country.svg\n
	reImageFlag = regexp.MustCompile(`image_flag\s+= (.+?)\n`)

	// capital = Capital\n
	reCapital = regexp.MustCompile(`capital\s+= (.+?)\n`)
)

var limit = rate.NewLimiter(rate.Every(time.Second), 2)

type Country struct {
	Name           string
	MapImageURL    string // image url
	FlagImageURL   string
	Capital        string
	AnswerLocation string // location answer, data from card.
}

var tmpls *template.Template

func init() {
	tmpls = template.Must(template.New("location").Parse(`Where in the world is **{{.Name}}**?
<!--question-->
{{.AnswerLocation}}

![Map of {{.Name}}]({{.MapImageURL}})`))
	tmpls = template.Must(tmpls.New("world").Parse(`Which country is this?

![Map of a country]({{.MapImageURL}})
<!--question-->
**{{.Name}}**`))
	tmpls = template.Must(tmpls.New("capital").Parse(`What is the capital of **{{.Name}}**?
<!--question-->
{{.Capital}}`))
	tmpls = template.Must(tmpls.New("flag").Parse(`Which country does this flag belong to?

![Flag of {{.Name}}]({{.FlagImageURL}})
<!--question-->
**{{.Name}}**`))
}

func get(url string) ([]byte, error) {
	if err := limit.Wait(context.Background()); err != nil {
		return nil, err
	}

	rsp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()

	if rsp.StatusCode != 200 {
		return nil, fmt.Errorf("%s %s", rsp.Status, url)
	}

	body, err := ioutil.ReadAll(rsp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func writeFile(r io.Reader, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func getPage(uname string) (io.Reader, error) {
	fname := "pages/" + uname + ".txt"
	if f, err := os.Open(fname); err == nil {
		defer f.Close()

		body, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(body), nil
	}

	url := "https://en.wikipedia.org/wiki/Special:Export/" + uname
	body, err := get(url)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(body), ioutil.WriteFile(fname, body, 0776)
}

func getWikiPage(uname string) (*wikiparse.Page, error) {
	f, err := getPage(uname)
	if err != nil {
		return nil, fmt.Errorf("get page error: %w", err)
	}
	p, err := wikiparse.NewParser(f)
	if err != nil {
		return nil, fmt.Errorf("parser error: %w", err)
	}
	page, err := p.Next()
	if err != nil {
		return nil, fmt.Errorf("page error: %w", err)
	}
	return page, nil
}

func toURLName(name string) string {
	return strings.Replace(name, " ", "_", -1)
}

func wikiFileURL(name string) string {
	uname := toURLName(name)
	m := md5.New()
	m.Write([]byte(uname))
	h := hex.EncodeToString(m.Sum(nil))
	// TODO: should path be escaped?
	return "https://upload.wikimedia.org/wikipedia/commons/" + string(h[0]) + "/" + h[0:2] + "/" + uname
}

func getFile(uname string) (io.Reader, error) {
	//fmt.Println("url", wikiFileURL(uname))
	fname := "files/" + uname
	if f, err := os.Open(fname); err == nil {
		defer f.Close()

		body, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(body), nil
	}

	url := wikiFileURL(uname)
	body, err := get(url)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(body), ioutil.WriteFile(fname, body, 0666)
}

func makeFile(dir, name string) error {
	r, err := getFile(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.Create(dir + "/" + name)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func makeTmpl(dir, name, tmpl string, data interface{}) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, name+".md")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpls.ExecuteTemplate(f, tmpl, data)
}

func readAnswer(dir, name string) (string, error) {
	path := filepath.Join(dir, name+".md")
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	ss := strings.Split(string(b), "<!--question-->")
	if len(ss) != 2 {
		return "", fmt.Errorf("missing %s answer", path)
	}
	return strings.TrimSpace(ss[1]), nil
}

// Try to parse a file link (there could be multiple).
func parseWikiFile(s string) string {
	//a := s
	const (
		fileTag     = "File:"
		itemFileTag = "[[File:"
	)
	if i := strings.Index(s, itemFileTag); i > -1 {
		s = s[i:]
		s = strings.TrimPrefix(s, itemFileTag)
		s = s[:strings.Index(s, "|")]
	} else if i := strings.Index(s, fileTag); i > -1 {
		s = s[i:]
		s = strings.TrimPrefix(s, fileTag) // At EOF
	}

	// Trim &lt->&gt comments.
	if i := strings.Index(s, "<"); i > -1 {
		j := strings.Index(s, ">")
		if j < i {
			panic(fmt.Sprintf("%s %v %v %s", s, i, j, "</>"))
		}
		s = s[:i] + s[j+1:]
	}

	// Trim {{!}} comments.
	if i := strings.Index(s, "{{!}}"); i > -1 {
		s = s[:i]
	}

	s = strings.TrimSpace(s)
	s = toURLName(s)
	//fmt.Println(a, "->", s)
	return s
}

// Try to parse the link.
func parseWikiLink(s string) string {
	const linkTag = "[["
	if i := strings.Index(s, linkTag); i > -1 {
		s = s[i:]
		s = strings.TrimPrefix(s, linkTag)
		s = s[:strings.Index(s, "]]")]
		s = strings.TrimSpace(s)
	}
	if i := strings.Index(s, "|"); i > -1 {
		s = s[i+1:]
	}
	return s
}

func run() error {
	// Setup caches
	os.Mkdir("pages", 0755)
	os.Mkdir("files", 0755)

	page, err := getWikiPage("Member_states_of_the_United_Nations")
	if err != nil {
		return err
	}

	var countries []string
	if *flagCountry != "" {
		countries = []string{*flagCountry}
	} else {
		vs := reCountry.FindAllStringSubmatch(page.Revisions[0].Text, -1)
		for _, v := range vs {
			countries = append(countries, v[1])
		}

		sort.Strings(countries)
		if err := ioutil.WriteFile("countries.txt", []byte(strings.Join(countries, "\n")), 0666); err != nil {
			return err
		}
	}
	fmt.Println("len:", len(countries))
	sort.Strings(countries)
	n := *flagPosition
	if n > 0 {
		countries = countries[n:]
	}

	for idx, name := range countries {
		uname := toURLName(name)
		fmt.Println(idx+n, ":", name)

		page, err := getWikiPage(uname)
		if err != nil {
			return err
		}

		// Follow redirects e.g. Bahamas -> The Bahamas.
		for page.Redir.Title != "" {
			uname = toURLName(page.Redir.Title)

			page, err = getWikiPage(uname)
			if err != nil {
				return err
			}
		}

		var mapName, flagName, capital string

		// Create Maps
		if x, ok := map[string]string{
			"Czech_Republic":  "EU-Czech_Republic.svg",
			"Myanmar":         "Myanmar_on_the_globe_(Myanmar_centered).svg",
			"North_Macedonia": "Europe-Republic_of_North_Macedonia.svg",
			"Eritrea":         "Eritrea_(Africa_orthographic_projection).svg", // Missing "Africa" in wikifile
			"Iceland":         "Iceland_(orthographic_projection).svg",        // Rename Island -> Iceland
		}[uname]; ok {
			mapName = x
		} else {
			v := reImageMap.FindStringSubmatch(page.Revisions[0].Text)
			if len(v) != 2 {
				v = reImageMap2.FindStringSubmatch(page.Revisions[0].Text)
				if len(v) != 2 {
					return fmt.Errorf("%v image map failed %v", name, v)
				}
			}
			mapName = parseWikiFile(v[1])
		}
		if err := makeFile("countries/images", mapName); err != nil {
			return err
		}

		// Create Flags
		if x, ok := map[string]string{
			"Federated_States_of_Micronesia": "Flag_of_the_Federated_States_of_Micronesia.svg", // Missing "the"
			"Honduras":                       "Flag_of_Honduras.svg",                           // Remove "_(darker_variant)"
			"Seychelles":                     "Flag_of_Seychelles.svg",                         // Remove "the" Seychelles
		}[uname]; ok {
			flagName = x
		} else {
			v := reImageFlag.FindStringSubmatch(page.Revisions[0].Text)
			if len(v) != 2 {
				return fmt.Errorf("%v image flag failed %v", name, v)
			}
			flagName = parseWikiFile(v[1])
		}
		if err := makeFile("countries/flags/images", flagName); err != nil {
			return err
		}

		if x, ok := map[string]string{
			"Bolivia":           "Sucre *(constitutional and judicial)* and La Paz *(executive and legislative)*",
			"Azerbaijan":        "Baku",
			"Equatorial_Guinea": "Malabo *(current) and Ciudad de la Paz *(under construction)*",
			"Eswatini":          "Mbabane *(executive)* and Lobamba *(legislative)*",
			"Ivory_Coast":       "Yamoussoukro *(de jure)* and Abidjan *(de facto)*",
			"Malaysia":          "Kuala Lumpur and Putrajaya *(administrative)*",
			"South_Africa":      "Pretoria *(executive)*, Cape Town *(legislative)* and Bloemfontein *(judicial)*",
			"Sri_Lanka":         "Sri Jayawardenepura Kotte *(legislative)* and Colombo *(executive and judicial)*",
			"Switzerland":       "None *(de jure)* and Bern *(de facto)*",
			"Yemen":             "Sana'a *(de jure)* and Aden *(Temporary capital)*",
			"United_States":     "Washington, D.C.",
		}[uname]; ok {
			capital = x
		} else {
			v := reCapital.FindStringSubmatch(page.Revisions[0].Text)
			if len(v) != 2 {
				return fmt.Errorf("%v capital failed %v", name, v)
			}
			capital = parseWikiLink(v[1])
		}

		// Load answer for location from card. To difficult to parse
		// automatically.
		ansLoc, err := readAnswer("countries", uname+"_location")
		if err != nil {
			return err
		}
		// Delete images to readd them...
		if strings.Contains(ansLoc, "![") {
			ansLoc = strings.Split(ansLoc, "![")[0]
			ansLoc = strings.TrimSpace(ansLoc)
		}

		country := Country{
			Name:           name,
			MapImageURL:    "images/" + mapName,
			FlagImageURL:   "images/" + flagName,
			Capital:        capital,
			AnswerLocation: ansLoc,
		}

		// Render the different files.
		if err := makeTmpl("countries", uname+"_location", "location", &country); err != nil {
			return err
		}
		if err := makeTmpl("countries", uname, "world", &country); err != nil {
			return err
		}
		if err := makeTmpl(filepath.Join("countries", "flags"), uname, "flag", &country); err != nil {
			return err
		}
		if err := makeTmpl(filepath.Join("countries", "capitals"), uname, "capital", &country); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
