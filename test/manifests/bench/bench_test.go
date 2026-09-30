package bench

import (
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/yaml"
)

func full() Params {
	return Params{RunID: "r1", ChainID: "bench-a", Commit: "abc123", Image: "seiload:v1",
		DurationMinutes: 10, ProfileCM: "seiload-profile-r1"}
}

func decode(t *testing.T, out []byte) batchv1.Job {
	t.Helper()
	var job batchv1.Job
	if err := yaml.Unmarshal(out, &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	return job
}

func TestRender_Defaults(t *testing.T) {
	g := NewWithT(t)
	out, err := Render(full())
	g.Expect(err).NotTo(HaveOccurred())
	job := decode(t, out)

	g.Expect(job.Name).To(Equal("seiload-r1"))
	g.Expect(job.Namespace).To(BeEmpty())
	g.Expect(job.Labels).To(HaveKeyWithValue("sei.io/harness-run", "r1"))
	g.Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64((10 + DeadlineSlackMinutes) * 60)))

	c := job.Spec.Template.Spec.Containers[0]
	g.Expect(c.Image).To(Equal("seiload:v1"))
	g.Expect(c.Args).To(ContainElement("--duration=10m"))
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	g.Expect(env).To(Equal(map[string]string{
		"SEILOAD_RUN_ID": "r1", "SEILOAD_CHAIN_ID": "bench-a",
		"SEILOAD_COMMIT_ID": "abc123", "SEILOAD_WORKLOAD": DefaultWorkload,
	}))
	g.Expect(job.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("seiload-profile-r1"))
}

func TestRender_Overrides(t *testing.T) {
	g := NewWithT(t)
	p := full()
	p.Namespace = "eng-x"
	p.Workload = "exp-42"
	p.DeadlineSeconds = 99
	out, err := Render(p)
	g.Expect(err).NotTo(HaveOccurred())
	job := decode(t, out)
	g.Expect(job.Namespace).To(Equal("eng-x"))
	g.Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(99)))
	g.Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(HaveField("Value", "exp-42")))
}

func TestRender_RejectsIncompleteParams(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Params)
		wants string
	}{
		{"missing run id", func(p *Params) { p.RunID = "" }, "runID"},
		{"missing chain id", func(p *Params) { p.ChainID = "" }, "chainID"},
		{"missing image", func(p *Params) { p.Image = "" }, "image"},
		{"missing profile", func(p *Params) { p.ProfileCM = "" }, "profileCM"},
		{"zero duration", func(p *Params) { p.DurationMinutes = 0 }, "durationMinutes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			p := full()
			tc.mut(&p)
			_, err := Render(p)
			g.Expect(err).To(MatchError(ContainSubstring(tc.wants)))
		})
	}
}

func TestRender_QuotesScalarParams(t *testing.T) {
	g := NewWithT(t)
	p := full()
	p.Workload = "123"
	p.Namespace = "true"
	out, err := Render(p)
	g.Expect(err).NotTo(HaveOccurred())
	job := decode(t, out)
	g.Expect(job.Namespace).To(Equal("true"))
	g.Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(HaveField("Value", "123")))
}

func TestRender_RejectsNegativeDeadline(t *testing.T) {
	g := NewWithT(t)
	p := full()
	p.DeadlineSeconds = -1
	_, err := Render(p)
	g.Expect(err).To(MatchError(ContainSubstring("deadlineSeconds")))
}
