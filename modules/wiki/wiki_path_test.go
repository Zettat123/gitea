package wiki

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/exp/rand"
)

func TestWebPathSegments(t *testing.T) {
	a := WebPathSegments("a%2Fa/b+c/d-e/f-g.-")
	assert.EqualValues(t, []string{"a/a", "b c", "d e", "f-g"}, a)
}

func TestUserTitleToWebPath(t *testing.T) {
	type test struct {
		Expected  string
		UserTitle string
	}
	for _, test := range []test{
		{"unnamed", ""},
		{"unnamed", "."},
		{"unnamed", ".."},
		{"wiki-name", "wiki name"},
		{"title.md.-", "title.md"},
		{"wiki-name.-", "wiki-name"},
		{"the+wiki-name.-", "the wiki-name"},
		{"a%2Fb", "a/b"},
		{"a%25b", "a%b"},
	} {
		assert.EqualValues(t, test.Expected, UserTitleToWebPath("", test.UserTitle))
	}
}

func TestWebPathToDisplayName(t *testing.T) {
	type test struct {
		Expected string
		WebPath  WebPath
	}
	for _, test := range []test{
		{"wiki name", "wiki-name"},
		{"wiki-name", "wiki-name.-"},
		{"name with / slash", "name-with %2F slash"},
		{"name with % percent", "name-with %25 percent"},
		{"2000-01-02 meeting", "2000-01-02+meeting.-.md"},
		{"a b", "a%20b.md"},
	} {
		_, displayName := WebPathToUserTitle(test.WebPath)
		assert.EqualValues(t, test.Expected, displayName)
	}
}

func TestWebPathToGitPath(t *testing.T) {
	type test struct {
		Expected string
		WikiName WebPath
	}
	for _, test := range []test{
		{"wiki-name.md", "wiki%20name"},
		{"wiki-name.md", "wiki+name"},
		{"wiki name.md", "wiki%20name.md"},
		{"wiki%20name.md", "wiki%2520name.md"},
		{"2000-01-02-meeting.md", "2000-01-02+meeting"},
		{"2000-01-02 meeting.-.md", "2000-01-02%20meeting.-"},
	} {
		assert.EqualValues(t, test.Expected, WebPathToGitPath(test.WikiName))
	}
}

func TestGitPathToWebPath(t *testing.T) {
	type test struct {
		Expected string
		Filename string
	}
	for _, test := range []test{
		{"hello-world", "hello-world.md"}, // this shouldn't happen, because it should always have a ".-" suffix
		{"hello-world", "hello world.md"},
		{"hello-world.-", "hello-world.-.md"},
		{"hello+world.-", "hello world.-.md"},
		{"symbols-%2F", "symbols %2F.md"},
	} {
		name, err := GitPathToWebPath(test.Filename)
		assert.NoError(t, err)
		assert.EqualValues(t, test.Expected, name)
	}
	for _, badFilename := range []string{
		"nofileextension",
		"wrongfileextension.txt",
	} {
		_, err := GitPathToWebPath(badFilename)
		assert.Error(t, err)
		assert.True(t, IsErrWikiInvalidFileName(err))
	}
	_, err := GitPathToWebPath("badescaping%%.md")
	assert.Error(t, err)
	assert.False(t, IsErrWikiInvalidFileName(err))
}

func TestUserWebGitPathConsistency(t *testing.T) {
	maxLen := 20
	b := make([]byte, maxLen)
	for i := 0; i < 1000; i++ {
		l := rand.Intn(maxLen)
		for j := 0; j < l; j++ {
			r := rand.Intn(0x80-0x20) + 0x20
			b[j] = byte(r)
		}

		userTitle := strings.TrimSpace(string(b[:l]))
		if userTitle == "" || userTitle == "." || userTitle == ".." {
			continue
		}
		webPath := UserTitleToWebPath("", userTitle)
		gitPath := WebPathToGitPath(webPath)

		webPath1, _ := GitPathToWebPath(gitPath)
		_, userTitle1 := WebPathToUserTitle(webPath1)
		gitPath1 := WebPathToGitPath(webPath1)

		assert.EqualValues(t, userTitle, userTitle1, "UserTitle for userTitle: %q", userTitle)
		assert.EqualValues(t, webPath, webPath1, "WebPath for userTitle: %q", userTitle)
		assert.EqualValues(t, gitPath, gitPath1, "GitPath for userTitle: %q", userTitle)
	}
}

func TestWebPathConversion(t *testing.T) {
	assert.Equal(t, "path/wiki", WebPathToURLPath(WebPath("path/wiki")))
	assert.Equal(t, "wiki", WebPathToURLPath(WebPath("wiki")))
	assert.Equal(t, "", WebPathToURLPath(WebPath("")))
}

func TestWebPathFromRequest(t *testing.T) {
	assert.Equal(t, WebPath("a%2Fb"), WebPathFromRequest("a/b"))
	assert.Equal(t, WebPath("a"), WebPathFromRequest("a"))
	assert.Equal(t, WebPath("b"), WebPathFromRequest("a/../b"))
}
