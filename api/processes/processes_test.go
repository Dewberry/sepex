package processes

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestJobDefIsPinned(t *testing.T) {
	tests := []struct {
		jobDef string
		want   bool
	}{
		{"gdal-ogrinfo:6", true},
		{"process-sandbox:2", true},
		{"arn:aws:batch:us-east-1:123456789012:job-definition/gdal-ogrinfo:6", true},

		{"gdal-ogrinfo", false},
		{"arn:aws:batch:us-east-1:123456789012:job-definition/gdal-ogrinfo", false},
		{"gdal-ogrinfo:", false},
		{"gdal-ogrinfo:latest", false},
		{"gdal-ogrinfo:6a", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := jobDefIsPinned(tt.jobDef); got != tt.want {
			t.Errorf("jobDefIsPinned(%q) = %v, want %v", tt.jobDef, got, tt.want)
		}
	}
}

func TestResourcesGPUsParsing(t *testing.T) {
	tests := []struct {
		name string
		yml  string
		want int
	}{
		{"declared", "cpus: 2\nmemory: 4096\ngpus: 2\n", 2},
		{"omitted defaults to none", "cpus: 2\nmemory: 4096\n", 0},
		{"explicit zero", "cpus: 2\nmemory: 4096\ngpus: 0\n", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Resources
			if err := yaml.Unmarshal([]byte(tt.yml), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.GPUs != tt.want {
				t.Errorf("GPUs = %d, want %d", got.GPUs, tt.want)
			}
		})
	}
}
