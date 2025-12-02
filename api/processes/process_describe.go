package processes

type processDescription struct {
	Info    `json:"info"`
	Command []string  `json:"command,omitempty"`
	Inputs  []Inputs  `json:"inputs"`
	Outputs []Outputs `json:"outputs"`
	Links   []Link    `json:"links"`
}

func (p Process) Describe() (processDescription, error) {
	pd := processDescription{
		Info: p.Info, Command: p.Command, Inputs: p.Inputs, Outputs: p.Outputs,
	} // Links: p.createLinks()

	return pd, nil
}
