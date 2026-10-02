// Package engine owns the model/tool loop and the mutable Engine and Scope
// lifetimes. It coordinates guarded effects, context maintenance and budgets.
// TurnKernel owns authoritative turn transitions; context owns context facts;
// contextview selects model-visible input; prompt assembles its content.
package engine
