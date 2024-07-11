package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

type Flag struct {
	Key             string
	MaintainerEmail string
	CreationDate    time.Time
	LastModified    time.Time
	LastRequested   time.Time
	Temporary       bool
}

func (f Flag) CreationDateMoreThan(value time.Duration) bool {
	return f.CreationDate.IsZero() || time.Since(f.CreationDate) > value
}

func (f Flag) LastModifiedMoreThan(value time.Duration) bool {
	return f.LastModified.IsZero() || time.Since(f.LastModified) > value
}

func (f Flag) LastRequestedMoreThan(value time.Duration) bool {
	return f.LastRequested.IsZero() || time.Since(f.LastRequested) > value
}

func (f Flag) CreationDateAgo() string {
	if f.CreationDate.IsZero() {
		return "never"
	}
	return f.ago(time.Since(f.CreationDate))
}

func (f Flag) LastModifiedAgo() string {
	if f.LastModified.IsZero() {
		return "never"
	}
	return f.ago(time.Since(f.LastModified))
}

func (f Flag) LastRequestedAgo() string {
	if f.LastRequested.IsZero() {
		return "never"
	}
	return f.ago(time.Since(f.LastRequested))
}

func (f Flag) ago(ago time.Duration) string {
	switch {
	case ago > 365*24*time.Hour:
		return fmt.Sprintf("%.1f years ago", float64(ago)/float64(24*time.Hour*365))
	case ago > 30*24*time.Hour:
		return fmt.Sprintf("%.1f months ago", float64(ago)/float64(24*time.Hour*30))
	case ago > 24*time.Hour:
		return fmt.Sprintf("%.1f days ago", float64(ago)/float64(24*time.Hour))
	case ago > time.Hour:
		return fmt.Sprintf("%.1f hours ago", float64(ago)/float64(time.Hour))
	case ago > time.Minute:
		return fmt.Sprintf("%.1f minutes ago", float64(ago)/float64(time.Minute))
	default:
		return fmt.Sprintf("%.0f seconds ago", float64(ago)/float64(time.Second))
	}
}

func (f Flag) GetStatus(threshold time.Duration) string {
	if f.LastRequestedMoreThan(threshold) {
		return "inactive"
	}
	return "inuse"
}

func (f Flag) GetTemporary() string {
	if f.Temporary {
		return "temporary"
	}
	return "permanent"
}

type Client struct {
	Client    http.Client
	ApiKey    string
	Host      string
	FirstPage string
	QueryUrl  string
}

const (
	host = "https://app.launchdarkly.com"
)

func firstPage(project, env string) string {
	return "/api/v2/flags/" + project + "?limit=50&env=" + env + "&sort=creationDate&filter=state%3Alive"
}

func queryUrl(project string) string {
	return "/api/v2/projects/" + project + "/flag-statuses/queries"
}

func (cli *Client) get(ctx context.Context, url string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", host+url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", cli.ApiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := cli.Client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}

	return nil
}

func (cli *Client) post(ctx context.Context, url string, in, out interface{}) error {
	inBuffer := bytes.NewBuffer([]byte{})
	if err := json.NewEncoder(inBuffer).Encode(in); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", host+url, inBuffer)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", cli.ApiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("LD-API-Version", "beta")
	resp, err := cli.Client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}

	return nil
}

type GetResponse struct {
	Links struct {
		Next struct {
			Href string `json:"href"`
			Type string `json:"type"`
		} `json:"next"`
	} `json:"_links"`
	Items []struct {
		Key        string `json:"key"`
		Maintainer struct {
			Email string `json:"email"`
		} `json:"_maintainer"`
		Temporary    bool  `json:"temporary"`
		CreationDate int64 `json:"creationDate"`
		Environments map[string]struct {
			LastModified int64 `json:"lastModified"`
		} `json:"environments"`
	} `json:"items"`
}

func (r *GetResponse) Keys() []string {
	keys := []string{}
	for _, item := range r.Items {
		keys = append(keys, item.Key)
	}
	return keys
}

type PostResponse struct {
	Items []struct {
		Key          string `json:"key"`
		Environments map[string]struct {
			Name          string    `json:"name"`
			LastRequested time.Time `json:"lastRequested"`
		} `json:"environments"`
	} `json:"items"`
}

func (r *PostResponse) LastRequested(env string) map[string]time.Time {
	lastRequested := map[string]time.Time{}
	for _, item := range r.Items {
		if value, ok := item.Environments[env]; ok {
			lastRequested[item.Key] = value.LastRequested
		}
	}
	return lastRequested
}

func (cli *Client) GetFlags(ctx context.Context, project, env string) ([]Flag, error) {
	var flags []Flag
	var nextUrl string

	for url := firstPage(project, env); url != ""; url = nextUrl {
		var getResponse GetResponse
		if err := cli.get(ctx, url, &getResponse); err != nil {
			return nil, err
		}

		nextUrl = getResponse.Links.Next.Href

		var postResponse PostResponse
		if err := cli.post(ctx, queryUrl(project), map[string]interface{}{
			"environmentKeys": []string{env},
			"flagKeys":        getResponse.Keys(),
		}, &postResponse); err != nil {
			return nil, err
		}

		lastRequested := postResponse.LastRequested(env)

		for _, item := range getResponse.Items {
			maintainerEmail := item.Maintainer.Email
			if maintainerEmail == "" {
				maintainerEmail = "unknown"
			}

			flags = append(flags, Flag{
				Key:             item.Key,
				MaintainerEmail: maintainerEmail,
				CreationDate:    time.Unix(item.CreationDate/1000, item.CreationDate%1000*1000000),
				LastModified:    time.Unix(item.Environments[env].LastModified/1000, item.Environments[env].LastModified%1000*1000000),
				LastRequested:   lastRequested[item.Key],
				Temporary:       item.Temporary,
			})
		}
	}

	return flags, nil
}

func main() {
	var project, env, token string
	var threshold time.Duration
	var format string
	var withPermanent bool

	flag.StringVar(&project, "project", "default", "project to check")
	flag.StringVar(&env, "env", "production", "environment to check")
	flag.StringVar(&token, "token", "LAUNCH_DARKLY_API_TOKEN", "env-var name with api token to authorize")
	flag.DurationVar(&threshold, "threshold", 6*30*24*time.Hour, "threshold for last modified and last requested (half-year by default)")
	flag.StringVar(&format, "format", "text", "output format: text/markdown/csv")
	flag.BoolVar(&withPermanent, "with-permanent", false, "show permanent flags as well")
	flag.Parse()

	client := Client{
		Client: http.Client{Timeout: time.Minute},
		ApiKey: os.Getenv(token),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	flags, err := client.GetFlags(ctx, project, env)
	if err != nil {
		panic(fmt.Errorf("failed to get flags: %w", err))
	}

	filtered := []Flag{}
	for _, item := range flags {
		if !item.CreationDateMoreThan(threshold) {
			continue
		}
		if !item.LastModifiedMoreThan(threshold) {
			continue
		}
		if !item.Temporary && !withPermanent {
			continue
		}
		filtered = append(filtered, item)
	}
	flags = filtered

	sort.Slice(flags, func(i, j int) bool {
		if flags[i].MaintainerEmail != flags[j].MaintainerEmail {
			return flags[i].MaintainerEmail < flags[j].MaintainerEmail
		}

		inactivei := flags[i].LastRequestedMoreThan(threshold)
		inactivej := flags[j].LastRequestedMoreThan(threshold)
		if inactivei != inactivej {
			return inactivei
		}

		return flags[i].CreationDate.Unix() < flags[j].CreationDate.Unix()
	})

	args := func(f Flag) []interface{} {
		return []interface{}{
			f.Key,
			f.MaintainerEmail,
			f.CreationDateAgo(),
			f.LastModifiedAgo(),
			f.LastRequestedAgo(),
			f.GetStatus(threshold),
			f.GetTemporary(),
			host + "/" + project + "/" + env + "/features/" + f.Key,
		}
	}

	switch format {
	case "markdown":
		fmt.Printf("KEY | MAINTAINER | CREATION DATE | LAST MODIFIED | LAST REQUESTED | STATUS | TEMPORARY | LINK \n")
		fmt.Printf("----+------------+---------------+---------------+----------------+--------+-----------+------\n")
		for _, item := range flags {
			fmt.Printf("%s | %s | %s | %s | %s | %s | %s | %s\n", args(item)...)
		}
	case "csv":
		fmt.Println("KEY,MAINTAINER,CREATION DATE,LAST MODIFIED,LAST REQUESTED,STATUS,TEMPORARY,LINK")

		for _, item := range flags {
			fmt.Printf("%s,%s,%s,%s,%s,%s,%s,%s\n", args(item)...)
		}
	default:
		tb := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
		fmt.Fprintln(tb, "KEY\tMAINTAINER\tCREATION DATE\tLAST MODIFIED\tLAST REQUESTED\tSTATUS\tTEMPORARY\tLINK")

		for _, item := range flags {
			fmt.Fprintf(tb, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", args(item)...)
		}

		tb.Flush()
	}
}
