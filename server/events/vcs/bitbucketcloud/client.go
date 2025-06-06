package bitbucketcloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	validator "github.com/go-playground/validator/v10"
	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
)

type Client struct {
	HTTPClient  *http.Client
	Username    string
	Password    string
	BaseURL     string
	AtlantisURL string
}

// NewClient builds a bitbucket cloud client. atlantisURL is the
// URL for Atlantis that will be linked to from the build status icons. This
// linking is annoying because we don't have anywhere good to link but a URL is
// required.
func NewClient(httpClient *http.Client, username string, password string, atlantisURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		HTTPClient:  httpClient,
		Username:    username,
		Password:    password,
		BaseURL:     BaseURL,
		AtlantisURL: atlantisURL,
	}
}

var MY_UUID = ""

// GetModifiedFiles returns the names of files that were modified in the merge request
// relative to the repo root, e.g. parent/child/file.txt.
func (b *Client) GetModifiedFiles(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest) ([]string, error) {
	var files []string

	nextPageURL := fmt.Sprintf("%s/2.0/repositories/%s/pullrequests/%d/diffstat", b.BaseURL, repo.FullName, pull.Num)
	// We'll only loop 1000 times as a safety measure.
	maxLoops := 1000
	for i := 0; i < maxLoops; i++ {
		resp, err := b.makeRequest("GET", nextPageURL, nil)
		if err != nil {
			return nil, err
		}
		var diffStat DiffStat
		if err := json.Unmarshal(resp, &diffStat); err != nil {
			return nil, errors.Wrapf(err, "Could not parse response %q", string(resp))
		}
		if err := validator.New().Struct(diffStat); err != nil {
			return nil, errors.Wrapf(err, "API response %q was missing fields", string(resp))
		}
		for _, v := range diffStat.Values {
			if v.Old != nil {
				files = append(files, *v.Old.Path)
			}
			if v.New != nil {
				files = append(files, *v.New.Path)
			}
		}
		if diffStat.Next == nil || *diffStat.Next == "" {
			break
		}
		nextPageURL = *diffStat.Next
	}

	// Now ensure all files are unique.
	hash := make(map[string]bool)
	var unique []string
	for _, f := range files {
		if !hash[f] {
			unique = append(unique, f)
			hash[f] = true
		}
	}
	return unique, nil
}

// CreateComment creates a comment on the merge request.
func (b *Client) CreateComment(logger logging.SimpleLogging, repo models.Repo, pullNum int, comment string, _ string) error {
	// NOTE: I tried to find the maximum size of a comment for bitbucket.org but
	// I got up to 200k chars without issue so for now I'm not going to bother
	// to detect this.
	bodyBytes, err := json.Marshal(map[string]map[string]string{"content": {
		"raw": comment,
	}})
	if err != nil {
		return errors.Wrap(err, "json encoding")
	}
	path := fmt.Sprintf("%s/2.0/repositories/%s/pullrequests/%d/comments", b.BaseURL, repo.FullName, pullNum)
	_, err = b.makeRequest("POST", path, bytes.NewBuffer(bodyBytes))
	return err
}

// UpdateComment updates the body of a comment on the merge request.
func (b *Client) ReactToComment(_ logging.SimpleLogging, _ models.Repo, _ int, _ int64, _ string) error {
	// TODO: Bitbucket support for reactions
	return nil
}

func (b *Client) HidePrevCommandComments(logger logging.SimpleLogging, repo models.Repo, pullNum int, command string, _ string) error {
	// there is no way to hide comment, so delete them instead
	me, err := b.GetMyUUID()
	if err != nil {
		return errors.Wrapf(err, "Cannot get my uuid! Please check required scope of the auth token!")
	}
	logger.Debug("My bitbucket user UUID is: %s", me)

	comments, err := b.GetPullRequestComments(repo, pullNum)
	if err != nil {
		return err
	}

	for _, c := range comments {
		logger.Debug("Comment is %v", c.Content.Raw)
		if strings.EqualFold(*c.User.UUID, me) {
			// do the same crude filtering as github client does
			body := strings.Split(c.Content.Raw, "\n")
			logger.Debug("Body is %s", body)
			if len(body) == 0 {
				continue
			}
			firstLine := strings.ToLower(body[0])
			if strings.Contains(firstLine, strings.ToLower(command)) {
				// we found our old comment that references that command
				logger.Debug("Deleting comment with id %s", *c.ID)
				err = b.DeletePullRequestComment(repo, pullNum, *c.ID)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *Client) DeletePullRequestComment(repo models.Repo, pullNum int, commentId int) error {
	path := fmt.Sprintf("%s/2.0/repositories/%s/pullrequests/%d/comments/%d", b.BaseURL, repo.FullName, pullNum, commentId)
	_, err := b.makeRequest("DELETE", path, nil)
	if err != nil {
		return err
	}
	return nil
}

func (b *Client) GetPullRequestComments(repo models.Repo, pullNum int) (comments []PullRequestComment, err error) {
	path := fmt.Sprintf("%s/2.0/repositories/%s/pullrequests/%d/comments", b.BaseURL, repo.FullName, pullNum)
	res, err := b.makeRequest("GET", path, nil)
	if err != nil {
		return comments, err
	}

	var pulls PullRequestComments
	if err := json.Unmarshal(res, &pulls); err != nil {
		return comments, errors.Wrapf(err, "Could not parse response %q", string(res))
	}
	return pulls.Values, nil
}

func (b *Client) GetMyUUID() (uuid string, err error) {
	if MY_UUID == "" {
		path := fmt.Sprintf("%s/2.0/user", b.BaseURL)
		resp, err := b.makeRequest("GET", path, nil)

		if err != nil {
			return uuid, err
		}

		var user User
		if err := json.Unmarshal(resp, &user); err != nil {
			return uuid, errors.Wrapf(err, "Could not parse response %q", string(resp))
		}

		if err := validator.New().Struct(user); err != nil {
			return uuid, errors.Wrapf(err, "API response %q was missing a field", string(resp))
		}

		uuid = *user.UUID
		MY_UUID = uuid

		return uuid, nil
	} else {
		return MY_UUID, nil
	}
}

// PullIsApproved returns true if the merge request was approved.
func (b *Client) PullIsApproved(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest) (approvalStatus models.ApprovalStatus, err error) {
	path := fmt.Sprintf("%s/2.0/repositories/%s/pullrequests/%d", b.BaseURL, repo.FullName, pull.Num)
	resp, err := b.makeRequest("GET", path, nil)
	if err != nil {
		return approvalStatus, err
	}
	var pullResp PullRequest
	if err := json.Unmarshal(resp, &pullResp); err != nil {
		return approvalStatus, errors.Wrapf(err, "Could not parse response %q", string(resp))
	}
	if err := validator.New().Struct(pullResp); err != nil {
		return approvalStatus, errors.Wrapf(err, "API response %q was missing fields", string(resp))
	}
	authorUUID := *pullResp.Author.UUID
	for _, participant := range pullResp.Participants {
		// Bitbucket allows the author to approve their own pull request. This
		// defeats the purpose of approvals so we don't count that approval.
		if *participant.Approved && *participant.User.UUID != authorUUID {
			return models.ApprovalStatus{
				IsApproved: true,
			}, nil
		}
	}
	return approvalStatus, nil
}

// PullIsMergeable returns true if the merge request has no conflicts and can be merged.
func (b *Client) PullIsMergeable(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest, _ string, _ []string) (bool, error) {
	nextPageURL := fmt.Sprintf("%s/2.0/repositories/%s/pullrequests/%d/diffstat", b.BaseURL, repo.FullName, pull.Num)
	// We'll only loop 1000 times as a safety measure.
	maxLoops := 1000
	for i := 0; i < maxLoops; i++ {
		resp, err := b.makeRequest("GET", nextPageURL, nil)
		if err != nil {
			return false, err
		}
		var diffStat DiffStat
		if err := json.Unmarshal(resp, &diffStat); err != nil {
			return false, errors.Wrapf(err, "Could not parse response %q", string(resp))
		}
		if err := validator.New().Struct(diffStat); err != nil {
			return false, errors.Wrapf(err, "API response %q was missing fields", string(resp))
		}
		for _, v := range diffStat.Values {
			// These values are undocumented, found via manual testing.
			if *v.Status == "merge conflict" || *v.Status == "local deleted" {
				return false, nil
			}
		}
		if diffStat.Next == nil || *diffStat.Next == "" {
			break
		}
		nextPageURL = *diffStat.Next
	}
	return true, nil
}

// UpdateStatus updates the status of a commit.
func (b *Client) UpdateStatus(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest, status models.CommitStatus, src string, description string, url string) error {
	bbState := "FAILED"
	switch status {
	case models.PendingCommitStatus:
		bbState = "INPROGRESS"
	case models.SuccessCommitStatus:
		bbState = "SUCCESSFUL"
	case models.FailedCommitStatus:
		bbState = "FAILED"
	}

	logger.Info("Updating BitBucket commit status for '%s' to '%s'", src, bbState)

	// URL is a required field for bitbucket statuses. We default to the
	// Atlantis server's URL.
	if url == "" {
		url = b.AtlantisURL
	}

	// Ensure key has at most 40 characters
	if utf8.RuneCountInString(src) > 40 {
		src = fmt.Sprintf("%.37s...", src)
	}

	bodyBytes, err := json.Marshal(map[string]string{
		"key":         src,
		"url":         url,
		"state":       bbState,
		"description": description,
	})

	path := fmt.Sprintf("%s/2.0/repositories/%s/commit/%s/statuses/build", b.BaseURL, repo.FullName, pull.HeadCommit)
	if err != nil {
		return errors.Wrap(err, "json encoding")
	}
	_, err = b.makeRequest("POST", path, bytes.NewBuffer(bodyBytes))
	return err
}

// MergePull merges the pull request.
func (b *Client) MergePull(logger logging.SimpleLogging, pull models.PullRequest, _ models.PullRequestOptions) error {
	path := fmt.Sprintf("%s/2.0/repositories/%s/pullrequests/%d/merge", b.BaseURL, pull.BaseRepo.FullName, pull.Num)
	_, err := b.makeRequest("POST", path, nil)
	return err
}

// MarkdownPullLink specifies the character used in a pull request comment.
func (b *Client) MarkdownPullLink(pull models.PullRequest) (string, error) {
	return fmt.Sprintf("#%d", pull.Num), nil
}

// prepRequest adds auth and necessary headers.
func (b *Client) prepRequest(method string, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(b.Username, b.Password)
	if body != nil {
		req.Header.Add("Content-Type", "application/json")
	}
	// Add this header to disable CSRF checks.
	// See https://confluence.atlassian.com/cloudkb/xsrf-check-failed-when-calling-cloud-apis-826874382.html
	req.Header.Add("X-Atlassian-Token", "no-check")
	return req, nil
}

func (b *Client) DiscardReviews(_ logging.SimpleLogging, _ models.Repo, _ models.PullRequest) error {
	// TODO implement
	return nil
}

func (b *Client) makeRequest(method string, path string, reqBody io.Reader) ([]byte, error) {
	req, err := b.prepRequest(method, path, reqBody)
	if err != nil {
		return nil, errors.Wrap(err, "constructing request")
	}
	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() // nolint: errcheck
	requestStr := fmt.Sprintf("%s %s", method, path)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("making request %q unexpected status code: %d, body: %s", requestStr, resp.StatusCode, string(respBody))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "reading response from request %q", requestStr)
	}
	return respBody, nil
}

// GetTeamNamesForUser returns the names of the teams or groups that the user belongs to (in the organization the repository belongs to).
func (b *Client) GetTeamNamesForUser(_ logging.SimpleLogging, _ models.Repo, _ models.User) ([]string, error) {
	return nil, nil
}

func (b *Client) SupportsSingleFileDownload(models.Repo) bool {
	return false
}

// GetFileContent a repository file content from VCS (which support fetch a single file from repository)
// The first return value indicates whether the repo contains a file or not
// if BaseRepo had a file, its content will placed on the second return value
func (b *Client) GetFileContent(_ logging.SimpleLogging, _ models.PullRequest, _ string) (bool, []byte, error) {
	return false, []byte{}, fmt.Errorf("not implemented")
}

func (b *Client) GetCloneURL(_ logging.SimpleLogging, _ models.VCSHostType, _ string) (string, error) {
	return "", fmt.Errorf("not yet implemented")
}

func (b *Client) GetPullLabels(_ logging.SimpleLogging, _ models.Repo, _ models.PullRequest) ([]string, error) {
	return nil, fmt.Errorf("not yet implemented")
}
