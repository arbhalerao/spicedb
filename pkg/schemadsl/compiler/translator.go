package compiler

import (
	"bufio"
	"fmt"
	"slices"
	"strings"

	"github.com/ccoveille/go-safecast"
	"github.com/jzelinskie/stringz"

	"github.com/authzed/spicedb/pkg/caveats"
	caveattypes "github.com/authzed/spicedb/pkg/caveats/types"
	"github.com/authzed/spicedb/pkg/namespace"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/dslshape"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
)

type translationContext struct {
	objectTypePrefix *string
	mapper           input.PositionMapper
	schemaString     string
	skipValidate     bool
	allowedFlags     []string
	enabledFlags     []string
	caveatTypeSet    *caveattypes.TypeSet
}

func (tctx *translationContext) prefixedPath(definitionName string) (string, error) {
	var prefix, name string
	if err := stringz.SplitInto(definitionName, "/", &prefix, &name); err != nil {
		if tctx.objectTypePrefix == nil {
			return "", fmt.Errorf("found reference `%s` without prefix", definitionName)
		}
		prefix = *tctx.objectTypePrefix
		name = definitionName
	}

	if prefix == "" {
		return name, nil
	}

	return stringz.Join("/", prefix, name), nil
}

const Ellipsis = "..."

func translate(tctx *translationContext, root *dslNode) (*CompiledSchema, error) {
	orderedDefinitions := make([]SchemaDefinition, 0, len(root.GetChildren()))
	var objectDefinitions []*core.NamespaceDefinition
	var caveatDefinitions []*core.CaveatDefinition

	nodes := make(map[string]*dslNode)

	for _, definitionNode := range root.GetChildren() {
		var definition SchemaDefinition

		switch definitionNode.GetType() {
		case dslshape.NodeTypeUseFlag:
			err := translateUseFlag(tctx, definitionNode)
			if err != nil {
				return nil, err
			}
			continue

		case dslshape.NodeTypeCaveatDefinition:
			def, err := translateCaveatDefinition(tctx, definitionNode)
			if err != nil {
				return nil, err
			}

			definition = def
			caveatDefinitions = append(caveatDefinitions, def)

		case dslshape.NodeTypeDefinition:
			def, err := translateObjectDefinition(tctx, definitionNode)
			if err != nil {
				return nil, err
			}

			definition = def
			objectDefinitions = append(objectDefinitions, def)
		}

		if _, ok := nodes[definition.GetName()]; ok {
			return nil, definitionNode.WithSourceErrorf(definition.GetName(), "found name reused between multiple definitions and/or caveats: %s", definition.GetName())
		}

		nodes[definition.GetName()] = definitionNode

		orderedDefinitions = append(orderedDefinitions, definition)
	}

	// Strip the type annotation metadata if typechecking isn't enabled.
	if !slices.Contains(tctx.enabledFlags, "typechecking") {
		for _, def := range objectDefinitions {
			for _, rel := range def.GetRelation() {
				err := namespace.SetTypeAnnotations(rel, nil)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	return &CompiledSchema{
		CaveatDefinitions:  caveatDefinitions,
		ObjectDefinitions:  objectDefinitions,
		OrderedDefinitions: orderedDefinitions,
		rootNode:           root,
		mapper:             tctx.mapper,
	}, nil
}

func translateCaveatDefinition(tctx *translationContext, defNode *dslNode) (*core.CaveatDefinition, error) {
	definitionName, err := defNode.GetString(dslshape.NodeCaveatDefinitionPredicateName)
	if err != nil {
		return nil, defNode.WithSourceErrorf(definitionName, "invalid definition name: %w", err)
	}

	// parameters
	paramNodes := defNode.List(dslshape.NodeCaveatDefinitionPredicateParameters)
	if len(paramNodes) == 0 {
		return nil, defNode.WithSourceErrorf(definitionName, "caveat `%s` must have at least one parameter defined", definitionName)
	}

	env := caveats.NewEnvironmentWithTypeSet(tctx.caveatTypeSet)
	parameters := make(map[string]caveattypes.VariableType, len(paramNodes))
	for _, paramNode := range paramNodes {
		paramName, err := paramNode.GetString(dslshape.NodeCaveatParameterPredicateName)
		if err != nil {
			return nil, paramNode.WithSourceErrorf(paramName, "invalid parameter name: %w", err)
		}

		if _, ok := parameters[paramName]; ok {
			return nil, paramNode.WithSourceErrorf(paramName, "duplicate parameter `%s` defined on caveat `%s`", paramName, definitionName)
		}

		typeRefNode, err := paramNode.Lookup(dslshape.NodeCaveatParameterPredicateType)
		if err != nil {
			return nil, paramNode.WithSourceErrorf(paramName, "invalid type for parameter: %w", err)
		}

		translatedType, err := translateCaveatTypeReference(tctx, typeRefNode)
		if err != nil {
			return nil, paramNode.WithSourceErrorf(paramName, "invalid type for caveat parameter `%s` on caveat `%s`: %w", paramName, definitionName, err)
		}

		parameters[paramName] = *translatedType
		err = env.AddVariable(paramName, *translatedType)
		if err != nil {
			return nil, paramNode.WithSourceErrorf(paramName, "invalid type for caveat parameter `%s` on caveat `%s`: %w", paramName, definitionName, err)
		}
	}

	caveatPath, err := tctx.prefixedPath(definitionName)
	if err != nil {
		return nil, defNode.Errorf("%w", err)
	}

	// caveat expression.
	expressionStringNode, err := defNode.Lookup(dslshape.NodeCaveatDefinitionPredicateExpession)
	if err != nil {
		return nil, defNode.WithSourceErrorf(definitionName, "invalid expression: %w", err)
	}

	expressionString, err := expressionStringNode.GetString(dslshape.NodeCaveatExpressionPredicateExpression)
	if err != nil {
		return nil, defNode.WithSourceErrorf(expressionString, "invalid expression: %w", err)
	}

	rnge, err := expressionStringNode.Range(tctx.mapper)
	if err != nil {
		return nil, defNode.WithSourceErrorf(expressionString, "invalid expression: %w", err)
	}

	source, err := caveats.NewSource(expressionString, caveatPath)
	if err != nil {
		return nil, defNode.WithSourceErrorf(expressionString, "invalid expression: %w", err)
	}

	compiled, err := caveats.CompileCaveatWithSource(env, caveatPath, source, rnge.Start())
	if err != nil {
		return nil, expressionStringNode.WithSourceErrorf(expressionString, "invalid expression for caveat `%s`: %w", definitionName, err)
	}

	def, err := namespace.CompiledCaveatDefinition(env, caveatPath, compiled)
	if err != nil {
		return nil, err
	}

	def.Metadata = addComments(def.Metadata, defNode)
	def.SourcePosition = getSourcePosition(defNode, tctx.mapper)
	return def, nil
}

func translateCaveatTypeReference(tctx *translationContext, typeRefNode *dslNode) (*caveattypes.VariableType, error) {
	typeName, err := typeRefNode.GetString(dslshape.NodeCaveatTypeReferencePredicateType)
	if err != nil {
		return nil, typeRefNode.WithSourceErrorf(typeName, "invalid type name: %w", err)
	}

	childTypeNodes := typeRefNode.List(dslshape.NodeCaveatTypeReferencePredicateChildTypes)
	childTypes := make([]caveattypes.VariableType, 0, len(childTypeNodes))
	for _, childTypeNode := range childTypeNodes {
		translated, err := translateCaveatTypeReference(tctx, childTypeNode)
		if err != nil {
			return nil, err
		}
		childTypes = append(childTypes, *translated)
	}

	constructedType, err := tctx.caveatTypeSet.BuildType(typeName, childTypes)
	if err != nil {
		return nil, typeRefNode.WithSourceErrorf(typeName, "%w", err)
	}

	return constructedType, nil
}

func translateObjectDefinition(tctx *translationContext, defNode *dslNode) (*core.NamespaceDefinition, error) {
	definitionName, err := defNode.GetString(dslshape.NodeDefinitionPredicateName)
	if err != nil {
		return nil, defNode.WithSourceErrorf(definitionName, "invalid definition name: %w", err)
	}

	relationsAndPermissions := []*core.Relation{}
	for _, relationOrPermissionNode := range defNode.GetChildren() {
		if relationOrPermissionNode.GetType() == dslshape.NodeTypeComment {
			continue
		}

		relationOrPermission, err := translateRelationOrPermission(tctx, relationOrPermissionNode)
		if err != nil {
			return nil, err
		}

		relationsAndPermissions = append(relationsAndPermissions, relationOrPermission)
	}

	nspath, err := tctx.prefixedPath(definitionName)
	if err != nil {
		return nil, defNode.Errorf("%w", err)
	}

	if len(relationsAndPermissions) == 0 {
		ns := namespace.Namespace(nspath)
		ns.Metadata = addComments(ns.Metadata, defNode)
		ns.SourcePosition = getSourcePosition(defNode, tctx.mapper)

		if !tctx.skipValidate {
			if err = ns.Validate(); err != nil {
				return nil, defNode.Errorf("error in object definition %s: %w", nspath, err)
			}
		}

		return ns, nil
	}

	ns := namespace.Namespace(nspath, relationsAndPermissions...)
	ns.Metadata = addComments(ns.Metadata, defNode)
	ns.SourcePosition = getSourcePosition(defNode, tctx.mapper)

	if !tctx.skipValidate {
		if err := ns.Validate(); err != nil {
			return nil, defNode.Errorf("error in object definition %s: %w", nspath, err)
		}
	}

	return ns, nil
}

func getSourcePosition(dslNode *dslNode, mapper input.PositionMapper) *core.SourcePosition {
	if !dslNode.Has(dslshape.NodePredicateStartRune) {
		return nil
	}

	sourceRange, err := dslNode.Range(mapper)
	if err != nil {
		return nil
	}

	line, col, err := sourceRange.Start().LineAndColumn()
	if err != nil {
		return nil
	}

	// We're okay with these being zero if the cast fails.
	uintLine, _ := safecast.ToUint64(line)
	uintCol, _ := safecast.ToUint64(col)

	return &core.SourcePosition{
		ZeroIndexedLineNumber:     uintLine,
		ZeroIndexedColumnPosition: uintCol,
	}
}

func addComments(mdmsg *core.Metadata, dslNode *dslNode) *core.Metadata {
	for _, child := range dslNode.GetChildren() {
		if child.GetType() == dslshape.NodeTypeComment {
			value, err := child.GetString(dslshape.NodeCommentPredicateValue)
			if err == nil {
				mdmsg, _ = namespace.AddComment(mdmsg, normalizeComment(value))
			}
		}
	}
	return mdmsg
}

func normalizeComment(value string) string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(value))
	for scanner.Scan() {
		trimmed := strings.TrimSpace(scanner.Text())
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, "\n")
}

func translateRelationOrPermission(tctx *translationContext, relOrPermNode *dslNode) (*core.Relation, error) {
	switch relOrPermNode.GetType() {
	case dslshape.NodeTypeRelation:
		rel, err := translateRelation(tctx, relOrPermNode)
		if err != nil {
			return nil, err
		}
		rel.Metadata = addComments(rel.Metadata, relOrPermNode)
		rel.SourcePosition = getSourcePosition(relOrPermNode, tctx.mapper)
		return rel, err

	case dslshape.NodeTypePermission:
		rel, err := translatePermission(tctx, relOrPermNode)
		if err != nil {
			return nil, err
		}
		rel.Metadata = addComments(rel.Metadata, relOrPermNode)
		rel.SourcePosition = getSourcePosition(relOrPermNode, tctx.mapper)
		return rel, err

	default:
		return nil, relOrPermNode.Errorf("unknown definition top-level node type %s", relOrPermNode.GetType())
	}
}

func translateRelation(tctx *translationContext, relationNode *dslNode) (*core.Relation, error) {
	relationName, err := relationNode.GetString(dslshape.NodePredicateName)
	if err != nil {
		return nil, relationNode.Errorf("invalid relation name: %w", err)
	}

	allowedDirectTypes := []*core.AllowedRelation{}
	for _, typeRef := range relationNode.List(dslshape.NodeRelationPredicateAllowedTypes) {
		allowedRelations, err := translateAllowedRelations(tctx, typeRef)
		if err != nil {
			return nil, err
		}

		allowedDirectTypes = append(allowedDirectTypes, allowedRelations...)
	}

	relation, err := namespace.Relation(relationName, nil, allowedDirectTypes...)
	if err != nil {
		return nil, err
	}

	if !tctx.skipValidate {
		if err := relation.Validate(); err != nil {
			return nil, relationNode.Errorf("error in relation %s: %w", relationName, err)
		}
	}

	return relation, nil
}

func translatePermission(tctx *translationContext, permissionNode *dslNode) (*core.Relation, error) {
	permissionName, err := permissionNode.GetString(dslshape.NodePredicateName)
	if err != nil {
		return nil, permissionNode.Errorf("invalid permission name: %w", err)
	}

	// Check for optional type annotations
	var typeAnnotations []string
	typeAnnotationNode, err := permissionNode.Lookup(dslshape.NodePermissionPredicateTypeAnnotations)
	if err == nil {
		annotations, err := extractTypeAnnotations(typeAnnotationNode)
		if err != nil {
			return nil, permissionNode.Errorf("error extracting type annotations: %w", err)
		}
		typeAnnotations = annotations
	}

	expressionNode, err := permissionNode.Lookup(dslshape.NodePermissionPredicateComputeExpression)
	if err != nil {
		return nil, permissionNode.Errorf("invalid permission expression: %w", err)
	}

	rewrite, err := translateExpression(tctx, expressionNode)
	if err != nil {
		return nil, err
	}

	permission, err := namespace.Relation(permissionName, rewrite)
	if err != nil {
		return nil, err
	}

	// Store type annotations in metadata
	if len(typeAnnotations) > 0 {
		err = namespace.SetTypeAnnotations(permission, typeAnnotations)
		if err != nil {
			return nil, permissionNode.Errorf("error adding type annotations to metadata: %w", err)
		}
	}

	if !tctx.skipValidate {
		if err := permission.Validate(); err != nil {
			return nil, permissionNode.Errorf("error in permission %s: %w", permissionName, err)
		}
	}

	return permission, nil
}

// extractTypeAnnotations is a helper function to return the literal identifiers under the type annotation node
func extractTypeAnnotations(typeAnnotationNode *dslNode) ([]string, error) {
	children := typeAnnotationNode.List(dslshape.NodeTypeAnnotationPredicateTypes)

	annotations := make([]string, 0, len(children))

	for _, child := range children {
		typeName, err := child.GetString(dslshape.NodeIdentiferPredicateValue)
		if err != nil {
			return nil, err
		}
		annotations = append(annotations, typeName)
	}

	return annotations, nil
}

func translateBinary(tctx *translationContext, expressionNode *dslNode) (*core.SetOperation_Child, *core.SetOperation_Child, error) {
	leftChild, err := expressionNode.Lookup(dslshape.NodeExpressionPredicateLeftExpr)
	if err != nil {
		return nil, nil, err
	}

	rightChild, err := expressionNode.Lookup(dslshape.NodeExpressionPredicateRightExpr)
	if err != nil {
		return nil, nil, err
	}

	leftOperation, err := translateExpressionOperation(tctx, leftChild)
	if err != nil {
		return nil, nil, err
	}

	rightOperation, err := translateExpressionOperation(tctx, rightChild)
	if err != nil {
		return nil, nil, err
	}

	return leftOperation, rightOperation, nil
}

func translateExpression(tctx *translationContext, expressionNode *dslNode) (*core.UsersetRewrite, error) {
	translated, err := translateExpressionDirect(tctx, expressionNode)
	if err != nil {
		return translated, err
	}

	translated.SourcePosition = getSourcePosition(expressionNode, tctx.mapper)
	return translated, nil
}

func collapseOps(op *core.SetOperation_Child, handler func(rewrite *core.UsersetRewrite) *core.SetOperation) []*core.SetOperation_Child {
	if op.GetUsersetRewrite() == nil {
		return []*core.SetOperation_Child{op}
	}

	usersetRewrite := op.GetUsersetRewrite()
	operation := handler(usersetRewrite)
	if operation == nil {
		return []*core.SetOperation_Child{op}
	}

	collapsed := make([]*core.SetOperation_Child, 0, len(operation.Child))
	for _, child := range operation.Child {
		collapsed = append(collapsed, collapseOps(child, handler)...)
	}
	return collapsed
}

func translateExpressionDirect(tctx *translationContext, expressionNode *dslNode) (*core.UsersetRewrite, error) {
	// For union and intersection, we collapse a tree of binary operations into a flat list containing child
	// operations of the *same* type.
	translate := func(
		builder func(firstChild *core.SetOperation_Child, rest ...*core.SetOperation_Child) *core.UsersetRewrite,
		lookup func(rewrite *core.UsersetRewrite) *core.SetOperation,
	) (*core.UsersetRewrite, error) {
		leftOperation, rightOperation, err := translateBinary(tctx, expressionNode)
		if err != nil {
			return nil, err
		}
		leftOps := collapseOps(leftOperation, lookup)
		rightOps := collapseOps(rightOperation, lookup)
		ops := append(leftOps, rightOps...)
		return builder(ops[0], ops[1:]...), nil
	}

	switch expressionNode.GetType() {
	case dslshape.NodeTypeUnionExpression:
		return translate(namespace.Union, func(rewrite *core.UsersetRewrite) *core.SetOperation {
			return rewrite.GetUnion()
		})

	case dslshape.NodeTypeIntersectExpression:
		return translate(namespace.Intersection, func(rewrite *core.UsersetRewrite) *core.SetOperation {
			return rewrite.GetIntersection()
		})

	case dslshape.NodeTypeExclusionExpression:
		// Order matters for exclusions, so do not perform the optimization.
		leftOperation, rightOperation, err := translateBinary(tctx, expressionNode)
		if err != nil {
			return nil, err
		}
		return namespace.Exclusion(leftOperation, rightOperation), nil

	default:
		op, err := translateExpressionOperation(tctx, expressionNode)
		if err != nil {
			return nil, err
		}

		return namespace.Union(op), nil
	}
}

func translateExpressionOperation(tctx *translationContext, expressionOpNode *dslNode) (*core.SetOperation_Child, error) {
	translated, err := translateExpressionOperationDirect(tctx, expressionOpNode)
	if err != nil {
		return translated, err
	}

	translated.SourcePosition = getSourcePosition(expressionOpNode, tctx.mapper)
	return translated, nil
}

func translateExpressionOperationDirect(tctx *translationContext, expressionOpNode *dslNode) (*core.SetOperation_Child, error) {
	switch expressionOpNode.GetType() {
	case dslshape.NodeTypeIdentifier:
		referencedRelationName, err := expressionOpNode.GetString(dslshape.NodeIdentiferPredicateValue)
		if err != nil {
			return nil, err
		}

		return namespace.ComputedUserset(referencedRelationName), nil

	case dslshape.NodeTypeNilExpression:
		return namespace.Nil(), nil

	case dslshape.NodeTypeArrowExpression:
		leftChild, err := expressionOpNode.Lookup(dslshape.NodeExpressionPredicateLeftExpr)
		if err != nil {
			return nil, err
		}

		rightChild, err := expressionOpNode.Lookup(dslshape.NodeExpressionPredicateRightExpr)
		if err != nil {
			return nil, err
		}

		if leftChild.GetType() != dslshape.NodeTypeIdentifier {
			return nil, leftChild.Errorf("Nested arrows not yet supported")
		}

		tuplesetRelation, err := leftChild.GetString(dslshape.NodeIdentiferPredicateValue)
		if err != nil {
			return nil, err
		}

		usersetRelation, err := rightChild.GetString(dslshape.NodeIdentiferPredicateValue)
		if err != nil {
			return nil, err
		}

		if expressionOpNode.Has(dslshape.NodeArrowExpressionFunctionName) {
			functionName, err := expressionOpNode.GetString(dslshape.NodeArrowExpressionFunctionName)
			if err != nil {
				return nil, err
			}

			return namespace.MustFunctionedTupleToUserset(tuplesetRelation, functionName, usersetRelation), nil
		}

		return namespace.TupleToUserset(tuplesetRelation, usersetRelation), nil

	case dslshape.NodeTypeUnionExpression:
		fallthrough

	case dslshape.NodeTypeIntersectExpression:
		fallthrough

	case dslshape.NodeTypeExclusionExpression:
		rewrite, err := translateExpression(tctx, expressionOpNode)
		if err != nil {
			return nil, err
		}
		return namespace.Rewrite(rewrite), nil

	default:
		return nil, expressionOpNode.Errorf("unknown expression node type %s", expressionOpNode.GetType())
	}
}

func translateAllowedRelations(tctx *translationContext, typeRefNode *dslNode) ([]*core.AllowedRelation, error) {
	switch typeRefNode.GetType() {
	case dslshape.NodeTypeTypeReference:
		references := []*core.AllowedRelation{}
		for _, subRefNode := range typeRefNode.List(dslshape.NodeTypeReferencePredicateType) {
			subReferences, err := translateAllowedRelations(tctx, subRefNode)
			if err != nil {
				return []*core.AllowedRelation{}, err
			}

			references = append(references, subReferences...)
		}
		return references, nil

	case dslshape.NodeTypeSpecificTypeReference:
		ref, err := translateSpecificTypeReference(tctx, typeRefNode)
		if err != nil {
			return []*core.AllowedRelation{}, err
		}
		return []*core.AllowedRelation{ref}, nil

	default:
		return nil, typeRefNode.Errorf("unknown type ref node type %s", typeRefNode.GetType())
	}
}

func translateSpecificTypeReference(tctx *translationContext, typeRefNode *dslNode) (*core.AllowedRelation, error) {
	typePath, err := typeRefNode.GetString(dslshape.NodeSpecificReferencePredicateType)
	if err != nil {
		return nil, typeRefNode.Errorf("invalid type name: %w", err)
	}

	nspath, err := tctx.prefixedPath(typePath)
	if err != nil {
		return nil, typeRefNode.Errorf("%w", err)
	}

	if typeRefNode.Has(dslshape.NodeSpecificReferencePredicateWildcard) {
		ref := &core.AllowedRelation{
			Namespace: nspath,
			RelationOrWildcard: &core.AllowedRelation_PublicWildcard_{
				PublicWildcard: &core.AllowedRelation_PublicWildcard{},
			},
		}

		err = addWithCaveats(tctx, typeRefNode, ref)
		if err != nil {
			return nil, typeRefNode.Errorf("invalid caveat: %w", err)
		}

		if !tctx.skipValidate {
			if err := ref.Validate(); err != nil {
				return nil, typeRefNode.Errorf("invalid type relation: %w", err)
			}
		}

		ref.SourcePosition = getSourcePosition(typeRefNode, tctx.mapper)
		return ref, nil
	}

	relationName := Ellipsis
	if typeRefNode.Has(dslshape.NodeSpecificReferencePredicateRelation) {
		relationName, err = typeRefNode.GetString(dslshape.NodeSpecificReferencePredicateRelation)
		if err != nil {
			return nil, typeRefNode.Errorf("invalid type relation: %w", err)
		}
	}

	ref := &core.AllowedRelation{
		Namespace: nspath,
		RelationOrWildcard: &core.AllowedRelation_Relation{
			Relation: relationName,
		},
	}

	// Add the caveat(s), if any.
	err = addWithCaveats(tctx, typeRefNode, ref)
	if err != nil {
		return nil, typeRefNode.Errorf("invalid caveat: %w", err)
	}

	// Add the expiration trait, if any.
	if traitNode, err := typeRefNode.Lookup(dslshape.NodeSpecificReferencePredicateTrait); err == nil {
		traitName, err := traitNode.GetString(dslshape.NodeTraitPredicateTrait)
		if err != nil {
			return nil, typeRefNode.Errorf("invalid trait: %w", err)
		}

		if traitName != "expiration" {
			return nil, typeRefNode.Errorf("invalid trait: %s", traitName)
		}

		if !slices.Contains(tctx.allowedFlags, "expiration") {
			return nil, typeRefNode.Errorf("expiration trait is not allowed")
		}

		ref.RequiredExpiration = &core.ExpirationTrait{}
	}

	if !tctx.skipValidate {
		if err := ref.Validate(); err != nil {
			return nil, typeRefNode.Errorf("invalid type relation: %w", err)
		}
	}

	ref.SourcePosition = getSourcePosition(typeRefNode, tctx.mapper)
	return ref, nil
}

func addWithCaveats(tctx *translationContext, typeRefNode *dslNode, ref *core.AllowedRelation) error {
	caveats := typeRefNode.List(dslshape.NodeSpecificReferencePredicateCaveat)
	if len(caveats) == 0 {
		return nil
	}

	if len(caveats) != 1 {
		return fmt.Errorf("only one caveat is currently allowed per type reference")
	}

	name, err := caveats[0].GetString(dslshape.NodeCaveatPredicateCaveat)
	if err != nil {
		return err
	}

	nspath, err := tctx.prefixedPath(name)
	if err != nil {
		return err
	}

	ref.RequiredCaveat = &core.AllowedCaveat{
		CaveatName: nspath,
	}
	return nil
}

// Translate use node and add flag to list of enabled flags
func translateUseFlag(tctx *translationContext, useFlagNode *dslNode) error {
	flagName, err := useFlagNode.GetString(dslshape.NodeUseFlagPredicateName)
	if err != nil {
		return err
	}
	if slices.Contains(tctx.enabledFlags, flagName) {
		return useFlagNode.Errorf("found duplicate use flag: %s", flagName)
	}
	tctx.enabledFlags = append(tctx.enabledFlags, flagName)
	return nil
}
