package types

type RootResolverTypeDefinition struct{}

func (*RootResolverTypeDefinition) Kind() string   { return "ROOT_RESOLVER" }
func (*RootResolverTypeDefinition) String() string { panic("RootResolver should be ignored") }
