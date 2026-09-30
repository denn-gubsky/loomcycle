package loop

// ContextOpIsStatic exposes the Context op classification to the external
// test package, which can read the real Context tool's op list (an in-package
// import of builtin would be a cycle).
var ContextOpIsStatic = contextOpIsStatic
