package modules

// Module defines a system query module that collects information.
type Module interface {
	Name() string
	Collect() (map[string]any, error)
}

// Registry returns all registered modules keyed by name.
func Registry() map[string]Module {
	mods := []Module{
		&DiskModule{},
		&CPUModule{},
		&MemoryModule{},
		&OSInfoModule{},
	}
	reg := make(map[string]Module, len(mods))
	for _, m := range mods {
		reg[m.Name()] = m
	}
	return reg
}

// CollectModules runs the named modules and returns their merged results.
// Each module's output is nested under its name. If names is empty, all
// modules are collected.
func CollectModules(names []string) map[string]any {
	reg := Registry()

	if len(names) == 0 {
		names = make([]string, 0, len(reg))
		for n := range reg {
			names = append(names, n)
		}
	}

	results := make(map[string]any, len(names))
	for _, name := range names {
		mod, ok := reg[name]
		if !ok {
			continue
		}
		data, err := mod.Collect()
		if err != nil {
			results[name] = map[string]any{"error": err.Error()}
			continue
		}
		results[name] = data
	}
	return results
}
