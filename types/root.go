package types

type RootResolverTypeDefinition struct{}

func (*RootResolverTypeDefinition) String() string { panic("RootResolver should be ignored") }
func (*RootResolverTypeDefinition) Kind() string   { return "ROOT_RESOLVER" }
