package gitlab

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		want   Kind
	}{
		{200, KindOther}, // success shouldn't hit classify, but defensively
		{301, KindOther},
		{400, KindBadRequest},
		{401, KindAuth},
		{403, KindAuth},
		{404, KindNotFound},
		{408, KindTransient},
		{409, KindConflict},
		{422, KindBadRequest},
		{429, KindTransient},
		{500, KindTransient},
		{502, KindTransient},
		{503, KindTransient},
		{504, KindTransient},
		{599, KindTransient},
		{418, KindOther}, // I'm a teapot: not in our table
	}
	for _, tc := range cases {
		got := ClassifyStatus(tc.status)
		if got != tc.want {
			t.Errorf("ClassifyStatus(%d) = %s, want %s", tc.status, got, tc.want)
		}
	}
}

func TestKindString(t *testing.T) {
	cases := []struct {
		k    Kind
		want string
	}{
		{KindOther, "other"},
		{KindBadRequest, "bad_request"},
		{KindAuth, "auth"},
		{KindNotFound, "not_found"},
		{KindConflict, "conflict"},
		{KindTransient, "transient"},
		{Kind(99), "other"},
	}
	for _, tc := range cases {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.k, got, tc.want)
		}
	}
}

func TestError_ErrorString(t *testing.T) {
	e := &Error{
		Kind:       KindAuth,
		StatusCode: http.StatusUnauthorized,
		Method:     "GET",
		URL:        "https://gitlab.example.com/api/v4/projects/foo/merge_requests/1",
		Body:       "401 Unauthorized",
	}
	got := e.Error()
	for _, want := range []string{"GET", "https://gitlab.example.com", "auth", "(401)", "401 Unauthorized"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() missing %q\n%s", want, got)
		}
	}
}

func TestError_ErrorString_WithoutStatus(t *testing.T) {
	e := &Error{
		Kind:   KindOther,
		Method: "GET",
		URL:    "https://x",
		Body:   "boom",
		Cause:  errors.New("connection refused"),
	}
	got := e.Error()
	if strings.Contains(got, "()") {
		t.Errorf("expected no parens when StatusCode is 0, got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("expected cause in Error(), got %q", got)
	}
}

func TestError_Unwrap(t *testing.T) {
	cause := errors.New("inner")
	e := &Error{Kind: KindOther, Cause: cause}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is should reach wrapped cause")
	}
}

func TestError_Is_ByKind(t *testing.T) {
	e := &Error{Kind: KindAuth, StatusCode: http.StatusUnauthorized}
	target := &Error{Kind: KindAuth}
	if !errors.Is(e, target) {
		t.Errorf("expected KindAuth match")
	}
	other := &Error{Kind: KindNotFound}
	if errors.Is(e, other) {
		t.Errorf("unexpected KindNotFound match")
	}
	nonEErr := errors.New("not our type")
	if errors.Is(e, nonEErr) {
		t.Errorf("non-Error target should not match")
	}
}

func TestAsError(t *testing.T) {
	inner := &Error{Kind: KindNotFound}
	wrapped := fmt.Errorf("ctx: %w", inner)
	got := AsError(wrapped)
	if got != inner {
		t.Errorf("AsError did not unwrap to inner *Error")
	}
	if AsError(errors.New("plain")) != nil {
		t.Errorf("AsError should return nil for non-Error chains")
	}
	if AsError(nil) != nil {
		t.Errorf("AsError(nil) should return nil")
	}
}
