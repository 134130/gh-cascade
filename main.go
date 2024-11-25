package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/cli/go-gh/v2"
	"github.com/cli/safeexec"
	"github.com/fatih/color"
)

var (
	bold     = color.New(color.Bold).SprintFunc()
	white    = color.New(color.FgWhite).SprintFunc()
	hiBlack  = color.New(color.FgHiBlack).SprintFunc()
	hiYellow = color.New(color.FgHiYellow).SprintFunc()
	red      = color.New(color.FgRed).SprintFunc()
	green    = color.New(color.FgGreen).SprintFunc()
	blue     = color.New(color.FgBlue).SprintFunc()
	purple   = color.New(color.FgMagenta).SprintFunc()
)

var ErrNoDependOn = fmt.Errorf("no dependencies found")

var _ flag.Value = (*RepositoryFlag)(nil)

type RepositoryFlag string

func (r *RepositoryFlag) String() string {
	return string(*r)
}

func (r *RepositoryFlag) Set(s string) error {
	*r = RepositoryFlag(s)
	return nil
}

func main() {
	// client, err := api.DefaultRESTClient()
	// if err != nil {
	// 	fmt.Println(err)
	// 	return
	// }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	isDirty, err := IsCurrentBranchDirty(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, red("error:"), err)
		return

	}
	if isDirty {
		fmt.Fprintln(os.Stderr, red("x"), "current branch is dirty. please retry after stashing or committing your changes.")
		return
	}

	sp := spinner.New(spinner.CharSets[14], 40*time.Millisecond)
	defer sp.Stop()

	sp.Suffix = " Fetching pull requests..."
	sp.Start()

	defaultBranch, err := GetDefaultBranch(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("resolve default branch: %w", err))
		return
	}

	if err = FetchOriginBranch(ctx, defaultBranch); err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("fetch origin/%s branch: %w", defaultBranch, err))
		return
	}

	var pullRequests []PullRequest
	if pullRequests, err = ListPullRequests(ctx); err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("list pull requests: %w", err))
		return
	}
	sp.Stop()

	fmt.Fprintf(color.Output, "%s%s\n", green("✔"), " Fetching pull requests...")

	if len(pullRequests) == 0 {
		fmt.Fprintf(color.Output, "%s%s\n", green("✔"), " No open or draft pull requests found.")
		return
	} else {
		fmt.Fprintf(color.Output, "%s%s\n", green("✔"), fmt.Sprintf(" Found %d open or draft pull requests.", len(pullRequests)))
	}

	sp = spinner.New(spinner.CharSets[14], 40*time.Millisecond)
	sp.Suffix = " Rebasing pull requests..."
	sp.Start()
	defer sp.Stop()

	processedPullRequests := []ProcessedPullRequest{}

	dependOnRegexp := regexp.MustCompile(`(?i)depend(?:s|ed|ing)?\s+on:\s+#(\d+)`)
	for _, pr := range pullRequests {
		var dependOns []int

		for _, match := range dependOnRegexp.FindAllStringSubmatch(pr.Body, -1) {
			dependOns = append(dependOns, func() int {
				i, err := strconv.Atoi(match[1])
				if err != nil {
					panic(err)
				}
				return i
			}())
		}

		if err != nil {
			panic(fmt.Errorf("pull request number is not integer: %w", err))
		}

		if len(dependOns) == 0 {
			processedPullRequests = append(processedPullRequests, ProcessedPullRequest{
				PullRequest: pr,
				DependOns:   nil,
				Error:       ErrNoDependOn,
			})
			continue
		}

		if len(dependOns) > 1 {
			processedPullRequests = append(processedPullRequests, ProcessedPullRequest{
				PullRequest: pr,
				DependOns:   dependOns,
				Error:       fmt.Errorf("multiple dependencies found: %v", dependOns),
			})
			continue
		}

		dependOn := dependOns[0]
		dependedPullRequest, err := GetPullRequest(ctx, dependOn)
		if err != nil {
			processedPullRequests = append(processedPullRequests, ProcessedPullRequest{
				PullRequest: pr,
				DependOns:   dependOns,
				Error:       fmt.Errorf("failed to get depended PR #%d: %w", dependOn, err),
			})
			continue
		}

		if dependedPullRequest.State != "MERGED" {
			processedPullRequests = append(processedPullRequests, ProcessedPullRequest{
				PullRequest: pr,
				DependOns:   dependOns,
				Error:       fmt.Errorf("depended PR #%d is not merged", dependOn),
			})
			continue
		}

		if err = CheckoutToPullRequest(ctx, pr.Number); err != nil {
			processedPullRequests = append(processedPullRequests, ProcessedPullRequest{
				PullRequest: pr,
				DependOns:   dependOns,
				Error:       fmt.Errorf("failed to checkout to depended PR #%d: %w", dependOn, err),
			})
			continue
		}

		if err = RebaseOntoPullRequest(ctx, "origin/"+defaultBranch, dependedPullRequest.MergeCommit.Oid, pr.HeadRefName); err != nil {
			processedPullRequests = append(processedPullRequests, ProcessedPullRequest{
				PullRequest: pr,
				DependOns:   dependOns,
				Error:       fmt.Errorf("failed to rebase depended PR #%d: %w", dependOn, err),
			})
			continue
		}

		processedPullRequests = append(processedPullRequests, ProcessedPullRequest{
			PullRequest:         pr,
			DependOns:           dependOns,
			DependedPullRequest: dependedPullRequest,
			Error:               nil,
		})
	}

	sp.Stop()
	fmt.Fprintf(color.Output, "%s%s\n", green("✔"), " Rebasing pull requests...")

	fmt.Fprintf(color.Output, "\n%s\n", bold("Rebased pull requests"))
	for _, pr := range processedPullRequests {
		if pr.Error != nil {
			continue
		}

		var colorFn = color.New(getColor(pr.PullRequest)).SprintFunc()

		fmt.Fprintf(color.Output, "  %s ← %s\n", white(pr.BaseRefName), white(pr.HeadRefName))
		fmt.Fprintf(color.Output, "    └─ %s %s\n", colorFn(fmt.Sprintf("#%-4d", pr.Number)), pr.URL)
		colorFn = color.New(getColor(pr.PullRequest)).SprintFunc()
		fmt.Fprintf(color.Output, "       └─ %s %s\n", colorFn(fmt.Sprintf("#%-4d", pr.DependedPullRequest.Number)), pr.DependedPullRequest.URL)
	}

	fmt.Fprintf(color.Output, "\n%s\n", bold("Pull requests not rebased"))
	for _, pr := range processedPullRequests {
		if pr.Error == nil {
			continue
		}

		var colorFn func(a ...interface{}) string
		if pr.IsDraft {
			colorFn = hiBlack
		} else {
			// Opened pull request
			colorFn = green
		}

		fmt.Fprintf(color.Output, "  %s ← %s\n", white(pr.BaseRefName), white(pr.HeadRefName))
		fmt.Fprintf(color.Output, "    └─ %s %s\n", colorFn(fmt.Sprintf("#%-4d", pr.Number)), pr.URL)
		if errors.Is(pr.Error, ErrNoDependOn) {
			fmt.Fprintf(color.Output, "             %s\n", hiYellow(pr.Error))
		} else {
			fmt.Fprintf(color.Output, "             %s\n", red(pr.Error))
		}
	}

	ctx.Done()
}

// For more examples of using go-gh, see:
// https://github.com/cli/go-gh/blob/trunk/example_gh_test.go

type PullRequest struct {
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	Body        string `json:"body"`
	IsDraft     bool   `json:"isDraft"`
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	State       string `json:"state"`
	MergeCommit struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit,omitempty"`
}

func GetDefaultBranch(ctx context.Context) (string, error) {
	stdout, stderr, err := gh.ExecContext(ctx, "repo", "view", "--json", "defaultBranchRef")
	if err != nil {
		return "", err
	}

	if stderr.Len() > 0 {
		return "", err
	}

	var defaultBranch struct {
		DefaultBranchRef struct {
			Name string `json:"name"`
		}
	}

	if err = json.Unmarshal(stdout.Bytes(), &defaultBranch); err != nil {
		return "", err
	}

	return defaultBranch.DefaultBranchRef.Name, nil
}

func FetchOriginBranch(ctx context.Context, branch string) error {
	gitPath, err := safeexec.LookPath("git")
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, gitPath, "fetch", "origin", branch)
	if err = cmd.Run(); err != nil {
		return err
	}

	return nil
}

func ListPullRequests(ctx context.Context) ([]PullRequest, error) {
	stdout, stderr, err := gh.ExecContext(ctx, "pr", "list", "--author", "@me", "--state", "open", "--json", "baseRefName,body,headRefName,isDraft,number,title,url,mergeCommit,state")
	if err != nil {
		return nil, err
	}

	if stderr.Len() > 0 {
		return nil, err
	}

	pullRequests := []PullRequest{}
	if err = json.Unmarshal(stdout.Bytes(), &pullRequests); err != nil {
		return nil, err
	}

	return pullRequests, nil
}

func GetPullRequest(ctx context.Context, number int) (*PullRequest, error) {
	stdout, stderr, err := gh.ExecContext(ctx, "pr", "view", strconv.Itoa(number), "--json", "baseRefName,body,headRefName,isDraft,number,title,url,mergeCommit,state")

	if err != nil {
		return nil, err
	}

	if stderr.Len() > 0 {
		return nil, err
	}

	pullRequest := &PullRequest{}
	if err = json.Unmarshal(stdout.Bytes(), pullRequest); err != nil {
		return nil, err
	}

	return pullRequest, nil
}

func IsCurrentBranchDirty(ctx context.Context) (bool, error) {
	gitPath, err := safeexec.LookPath("git")
	if err != nil {
		return false, err
	}

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, gitPath, "status", "--porcelain")
	cmd.Stdout = &stdout

	if err = cmd.Run(); err != nil {
		return false, err
	}

	return stdout.Len() > 0, nil
}

func CheckoutToPullRequest(ctx context.Context, number int) error {
	ghPath, err := safeexec.LookPath("gh")
	if err != nil {
		return err
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ghPath, "pr", "checkout", strconv.Itoa(number))
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "CLICOLOR_FORCE=0")
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", stderr.String(), err)
	}

	return nil
}

func RebaseOntoPullRequest(ctx context.Context, targetBase, oldParent, topicBranch string) error {
	gitPath, err := safeexec.LookPath("git")
	if err != nil {
		return err
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitPath, "rebase", "--onto", targetBase, oldParent, topicBranch)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		_ = exec.CommandContext(ctx, gitPath, "rebase", "--abort").Run()

		if strings.Contains(stderr.String(), "could not apply") {
			return fmt.Errorf("conflicted while rebasing %s onto %s (old parent: %s)", topicBranch, targetBase, oldParent[:7])
		} else {
			return fmt.Errorf("%s: %w", stderr.String(), err)
		}
	}

	return nil
}

type ProcessedPullRequest struct {
	PullRequest
	DependOns           []int
	DependedPullRequest *PullRequest
	Error               error
}

func getColor(pullRequest PullRequest) color.Attribute {
	switch pullRequest.State {
	case "OPEN":
		if pullRequest.IsDraft {
			return color.FgHiBlack
		} else {
			return color.FgGreen
		}
	case "MERGED":
		return color.FgMagenta
	case "CLOSED":
		return color.FgRed
	default:
		return color.FgHiBlack
	}
}
