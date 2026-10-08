package benchmark

import (
	"errors"
	"strings"
	"testing"
)

func TestCaseEffectiveWithoutDiscussion(t *testing.T) {
	for _, tt := range []struct {
		name    string
		setting string
		pinned  bool
		want    bool
	}{
		{name: "pinned defaults isolated", pinned: true, want: true},
		{name: "unpinned defaults normal"},
		{name: "pinned explicit isolated", pinned: true, setting: "true", want: true},
		{name: "pinned explicit opt out", pinned: true, setting: "false"},
		{name: "unpinned explicit normal", setting: "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := validSuiteYAML()
			if !tt.pinned {
				body = strings.Replace(body, "    review_base_sha: 1111111\n    review_head_sha: 2222222\n", "", 1)
			}
			if tt.setting != "" {
				body = strings.Replace(body, "  - id: case1\n", "  - id: case1\n    without_discussion: "+tt.setting+"\n", 1)
			}
			suite := loadSuite(t, body)
			if err := Validate(suite, testConfig()); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := suite.Cases[0].EffectiveWithoutDiscussion(); got != tt.want {
				t.Fatalf("EffectiveWithoutDiscussion = %t, want %t", got, tt.want)
			}
			if (suite.Cases[0].WithoutDiscussion == nil) != (tt.setting == "") {
				t.Fatal("setting presence did not survive YAML loading")
			}
		})
	}
}

func TestValidateRejectsWithoutDiscussionForUnpinnedCase(t *testing.T) {
	body := strings.Replace(validSuiteYAML(), "    review_base_sha: 1111111\n    review_head_sha: 2222222\n", "    without_discussion: true\n", 1)
	err := Validate(loadSuite(t, body), testConfig())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "without_discussion requires review_base_sha and review_head_sha") {
		t.Fatalf("Validate error = %v, want pinned requirement", err)
	}
}

func TestLoadRejectsNonBooleanWithoutDiscussion(t *testing.T) {
	for _, value := range []string{"null", "", "\"true\"", "1", "[]", "{}"} {
		t.Run(value, func(t *testing.T) {
			body := strings.Replace(validSuiteYAML(), "  - id: case1\n", "  - id: case1\n    without_discussion: "+value+"\n", 1)
			_, err := Load([]byte(body))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "without_discussion") && !strings.Contains(err.Error(), "bool") {
				t.Fatalf("Load error = %v, want invalid boolean", err)
			}
		})
	}
}

func TestExpectedSHAsDoNotEnableWithoutDiscussion(t *testing.T) {
	benchCase := Case{ExpectedBaseSHA: "1111111", ExpectedHeadSHA: "2222222"}
	if benchCase.EffectiveWithoutDiscussion() {
		t.Fatal("expected SHA metadata enabled discussion isolation")
	}
}
