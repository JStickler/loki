package rules

import (
	"testing"

	"github.com/prometheus/prometheus/model/rulefmt"

	"github.com/grafana/loki/v3/pkg/tool/rules/rwrulefmt"
)

func Test_rulesEqual(t *testing.T) {
	tests := []struct {
		name string
		a    *rulefmt.Rule
		b    *rulefmt.Rule
		want bool
	}{
		{
			name: "rule_node_identical",
			a: &rulefmt.Rule{
				Record:      "one",
				Expr:        "up",
				Annotations: map[string]string{"a": "b", "c": "d"},
				Labels:      nil,
			},
			b: &rulefmt.Rule{
				Record:      "one",
				Expr:        "up",
				Annotations: map[string]string{"c": "d", "a": "b"},
				Labels:      nil,
			},
			want: true,
		},
		{
			name: "rule_node_diff",
			a: &rulefmt.Rule{
				Record: "one",
				Expr:   "up",
			},
			b: &rulefmt.Rule{
				Record: "two",
				Expr:   "up",
			},
			want: false,
		},
		{
			name: "rule_node_annotations_diff",
			a: &rulefmt.Rule{
				Record:      "one",
				Expr:        "up",
				Annotations: map[string]string{"a": "b"},
			},
			b: &rulefmt.Rule{
				Record:      "one",
				Expr:        "up",
				Annotations: map[string]string{"c": "d"},
			},
			want: false,
		},
		{
			name: "rule_node_annotations_nil_diff",
			a: &rulefmt.Rule{
				Record:      "one",
				Expr:        "up",
				Annotations: map[string]string{"a": "b"},
			},
			b: &rulefmt.Rule{
				Record:      "one",
				Expr:        "up",
				Annotations: nil,
			},
			want: false,
		},
		{
			name: "rule_node_yaml_diff",
			a: &rulefmt.Rule{
				Record: "one",
				Expr:   "up",
			},
			b: &rulefmt.Rule{
				Record: "one",
				Expr:   "up",
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rulesEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("rulesEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompareGroups(t *testing.T) {
	tests := []struct {
		name        string
		groupOne    rwrulefmt.RuleGroup
		groupTwo    rwrulefmt.RuleGroup
		expectedErr error
	}{
		{
			name: "identical configs",
			groupOne: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
			},
			groupTwo: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
			},
			expectedErr: nil,
		},
		{
			name: "different rule length",
			groupOne: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
			},
			groupTwo: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
			},
			expectedErr: errDiffRuleLen,
		},
		{
			name: "identical rw configs",
			groupOne: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
				RWConfigs: []rwrulefmt.RemoteWriteConfig{
					{URL: "localhost"},
				},
			},
			groupTwo: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
				RWConfigs: []rwrulefmt.RemoteWriteConfig{
					{URL: "localhost"},
				},
			},
			expectedErr: nil,
		},
		{
			name: "different rw config lengths",
			groupOne: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
				RWConfigs: []rwrulefmt.RemoteWriteConfig{
					{URL: "localhost"},
				},
			},
			groupTwo: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
				RWConfigs: []rwrulefmt.RemoteWriteConfig{
					{URL: "localhost"},
					{URL: "localhost"},
				},
			},
			expectedErr: errDiffRWConfigs,
		},
		{
			name: "different rw configs",
			groupOne: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
				RWConfigs: []rwrulefmt.RemoteWriteConfig{
					{URL: "localhost"},
				},
			},
			groupTwo: rwrulefmt.RuleGroup{
				RuleGroup: rulefmt.RuleGroup{
					Name: "example_group",
					Rules: []rulefmt.Rule{
						{
							Record:      "one",
							Expr:        "up",
							Annotations: map[string]string{"a": "b", "c": "d"},
							Labels:      nil,
						},
					},
				},
				RWConfigs: []rwrulefmt.RemoteWriteConfig{
					{URL: "localhost2"},
				},
			},
			expectedErr: errDiffRWConfigs,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CompareGroups(tt.groupOne, tt.groupTwo); err != nil {
				if err != tt.expectedErr {
					t.Errorf("CompareGroups() error = %v, wantErr %v", err, tt.expectedErr)
				}
			}
		})
	}
}
