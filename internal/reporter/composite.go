package reporter

import "neutron/internal/model"

type Composite struct {
	reporters []model.Reporter
}

func NewComposite(reporters ...model.Reporter) *Composite {
	return &Composite{
		reporters: reporters,
	}
}

func (r *Composite) Report(jobName string, stepName string, status model.StepResult, description string) {
	for _, reporter := range r.reporters {
		reporter.Report(jobName, stepName, status, description)
	}
}

func (r *Composite) ReportJobFinal(status model.StepResult, description string) {
	for _, reporter := range r.reporters {
		reporter.ReportJobFinal(status, description)
	}
}
