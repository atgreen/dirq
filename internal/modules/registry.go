// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

// Module defines a system query module that collects information.
type Module interface {
	Name() string
	Collect() (map[string]any, error)
}

// FilterableModule is optionally implemented by modules that can optimize
// collection when specific field values are known from the WHERE clause.
// For example, the packages module can run "rpm -q kernel" instead of
// "rpm -qa" when the filter specifies packages.name = 'kernel'.
type FilterableModule interface {
	Module
	CollectFiltered(nameHints []string) (map[string]any, error)
}

// Registry returns all registered modules keyed by name.
func Registry() map[string]Module {
	mods := []Module{
		&DiskModule{},
		&CPUModule{},
		&MemoryModule{},
		&OSInfoModule{},
		&PackagesModule{},
		&NetworkModule{},
		&ServicesModule{},
	}
	reg := make(map[string]Module, len(mods))
	for _, m := range mods {
		reg[m.Name()] = m
	}
	return reg
}

// ModuleHints provides per-module filter hints extracted from the query's
// WHERE clause. Keys are module names, values are known field values
// (e.g., {"packages": ["kernel", "openssl"]} for packages.name IN ...).
type ModuleHints map[string][]string

// CollectModules runs the named modules and returns their merged results.
// Each module's output is nested under its name. If names is empty, all
// modules are collected. Optional hints allow modules to optimize collection.
func CollectModules(names []string, hints ...ModuleHints) map[string]any {
	reg := Registry()

	if len(names) == 0 {
		names = make([]string, 0, len(reg))
		for n := range reg {
			names = append(names, n)
		}
	}

	var h ModuleHints
	if len(hints) > 0 {
		h = hints[0]
	}

	results := make(map[string]any, len(names))
	for _, name := range names {
		mod, ok := reg[name]
		if !ok {
			continue
		}

		var data map[string]any
		var err error

		// Use filtered collection if the module supports it and we have hints.
		if fm, ok := mod.(FilterableModule); ok && h != nil {
			if nameHints, exists := h[name]; exists && len(nameHints) > 0 {
				data, err = fm.CollectFiltered(nameHints)
			} else {
				data, err = mod.Collect()
			}
		} else {
			data, err = mod.Collect()
		}

		if err != nil {
			results[name] = map[string]any{"error": err.Error()}
			continue
		}
		results[name] = data
	}
	return results
}
