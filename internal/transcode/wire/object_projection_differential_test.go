package wire

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The expected value is constructed before unknown fields are injected. This
// oracle does not use the production field walk or its member-deletion logic.
func TestExactNameProjectionIndependent(t *testing.T) {
	type leaf struct {
		Status   string          `json:"status"`
		N        int             `json:"n"`
		Raw      json.RawMessage `json:"raw"`
		Metadata map[string]int  `json:"metadata"`
	}
	type document struct {
		Title  string          `json:"title"`
		Child  leaf            `json:"child"`
		List   []leaf          `json:"list"`
		ByName map[string]leaf `json:"by_name"`
	}
	random := rand.New(rand.NewSource(421))
	object := func(members []string) string {
		random.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
		return "{ \n\t" + strings.Join(members, " , \r\n ") + " \n}"
	}
	noise := func() string {
		return []string{`null`, `"ignored"`, `1e10000`, `{"X":1,"x":2}`, `[1,{"opaque":"data"}]`}[random.Intn(5)]
	}
	makeLeaf := func() (string, leaf) {
		var want leaf
		var members []string
		if random.Intn(2) == 0 {
			want.Status = "kept"
			members = append(members, `"\u0073tatus":"kept"`)
		}
		if random.Intn(2) == 0 {
			want.N = 19
			members = append(members, `"n":19`)
		}
		if random.Intn(2) == 0 {
			want.Raw = json.RawMessage(`{ "A":1, "a":2, "marker":"<\u0041>" }`)
			members = append(members, `"raw":`+string(want.Raw))
		}
		if random.Intn(2) == 0 {
			want.Metadata = map[string]int{"Status": 1, "status": 2, "n": 3}
			members = append(members, `"metadata":{"Status":1,"status":2,"n":3}`)
		}
		for _, key := range []string{"STATUS", "ſtatus", "N", "RAW", "METADATA", "provider"} {
			if random.Intn(2) == 0 {
				members = append(members, strconv.Quote(key)+":"+noise())
			}
		}
		return object(members), want
	}
	for i := range 1200 {
		var want document
		var members []string
		if random.Intn(2) == 0 {
			want.Title = "canonical"
			members = append(members, `"title":"canonical"`)
		}
		child, childWant := makeLeaf()
		want.Child = childWant
		members = append(members, `"child":`+child)
		var list []string
		for range 3 {
			body, value := makeLeaf()
			list = append(list, body)
			want.List = append(want.List, value)
		}
		members = append(members, `"list":[`+strings.Join(list, ",")+`]`)
		want.ByName = make(map[string]leaf)
		var named []string
		for _, key := range []string{"A", "a"} {
			body, value := makeLeaf()
			want.ByName[key] = value
			named = append(named, strconv.Quote(key)+":"+body)
		}
		members = append(members, `"by_name":`+object(named))
		for _, key := range []string{"TITLE", "CHILD", "LIST", "BY_NAME", "opaque"} {
			if random.Intn(2) == 0 {
				members = append(members, strconv.Quote(key)+":"+noise())
			}
		}
		input := object(members)
		var got document
		if err := DecodeTolerant([]byte(input), &got); err != nil {
			t.Fatalf("case %d: %v\n%s", i, err, input)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: exact projection changed retained data or bound unknown data\ngot=%s\nwant=%s\n%s", i, fmt.Sprintf("%#v", got), fmt.Sprintf("%#v", want), input)
		}
	}
	t.Log("checked 1200 nested documents, including 7200 typed leaf objects")
}
