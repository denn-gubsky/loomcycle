package codehook

import (
	"fmt"

	"github.com/dop251/goja"
)

// The sandbox bounds a body's time, not its memory, and goja checks the time
// budget between instructions, never inside a native builtin. So one call such
// as "x".repeat(1<<31) allocates GiBs in a single uninterruptible step and can
// take down the whole server. These caps refuse the one-call allocators up
// front. They are defence in depth, not a memory bound: a body that grows a
// string in a loop is still limited only by its time budget, which is why a
// tenant's code hooks need their own opt-in.
//
// The bounds sit far above anything a hook needs — its event and its decision
// are each at most 1 MiB of JSON — and far below what hurts:
const (
	// maxStringLength bounds a string repeat / padStart / padEnd produce, in
	// UTF-16 code units: 1 Mi, the size of the largest event a hook receives.
	maxStringLength = 1 << 20
	// maxArrayLength bounds constructing an array, Array.from, and the natives
	// that walk an array's whole length (join, fill). Growing .length cannot be
	// intercepted from JavaScript, and costs nothing until one of those walks
	// it, so they are where it is caught. 64 Ki elements.
	maxArrayLength = 1 << 16
	// maxBufferBytes bounds an ArrayBuffer or a typed array: 1 MiB.
	maxBufferBytes = 1 << 20
)

// limitsPrelude replaces the one-call allocators with checked wrappers. The
// real functions stay reachable only through the wrappers' closures, so a body
// that deletes or reassigns a wrapper cannot get the original back; every
// prototype's constructor is repointed for the same reason. %d slots: string,
// array and buffer bounds.
const limitsPrelude = `
(function (global) {
	var MAX_STRING = %d, MAX_ELEMENTS = %d, MAX_BYTES = %d;
	function refuse(what, n, max) {
		throw new RangeError(what + " of " + n + " exceeds a code hook's limit of " + max);
	}
	function define(obj, name, value) {
		Object.defineProperty(obj, name, {value: value, writable: true, configurable: true, enumerable: false});
	}
	function guard(obj, name, check) {
		var real = obj[name];
		define(obj, name, function () {
			check.apply(this, arguments);
			return real.apply(this, arguments);
		});
	}
	function arrayLength(n) {
		if (Number(n) > MAX_ELEMENTS) { refuse("an array length", n, MAX_ELEMENTS); }
	}

	var S = String.prototype;
	guard(S, "repeat", function (count) {
		var n = String(this).length * Number(count);
		if (n > MAX_STRING) { refuse("a string length", n, MAX_STRING); }
	});
	function pad(maxLength) {
		var n = Number(maxLength);
		if (n > MAX_STRING && n > String(this).length) { refuse("a string length", n, MAX_STRING); }
	}
	guard(S, "padStart", pad);
	guard(S, "padEnd", pad);

	var RealArray = Array;
	var realFrom = RealArray.from;
	function CappedArray() {
		if (arguments.length === 1 && typeof arguments[0] === "number") { arrayLength(arguments[0]); }
		return RealArray.apply(null, arguments);
	}
	CappedArray.prototype = RealArray.prototype;
	define(CappedArray, "isArray", RealArray.isArray);
	define(CappedArray, "of", RealArray.of);
	define(CappedArray, "from", function (src) {
		if (src !== null && src !== undefined) { arrayLength(src.length); }
		return realFrom.apply(RealArray, arguments);
	});
	define(RealArray.prototype, "constructor", CappedArray);
	guard(RealArray.prototype, "join", function () { arrayLength(this.length); });
	guard(RealArray.prototype, "fill", function () { arrayLength(this.length); });
	global.Array = CappedArray;

	var RealBuffer = ArrayBuffer;
	function bytes(what, a, per) {
		var n;
		if (a !== null && typeof a === "object") {
			if (a instanceof RealBuffer) { return; } // a view on an existing buffer
			n = Number(a.length) * per;
		} else {
			n = Number(a) * per;
		}
		if (n > MAX_BYTES) { refuse(what, n, MAX_BYTES); }
	}
	function capConstructor(name, per) {
		var Real = global[name];
		if (typeof Real !== "function") { return; }
		var Capped = function () {
			if (new.target === undefined) { throw new TypeError("Constructor " + name + " requires 'new'"); }
			bytes("a " + name + " of bytes", arguments[0], per);
			return Reflect.construct(Real, arguments);
		};
		Object.setPrototypeOf(Capped, Object.getPrototypeOf(Real)); // inherit from/of
		Capped.prototype = Real.prototype;
		define(Real.prototype, "constructor", Capped);
		if (Real.BYTES_PER_ELEMENT) { define(Capped, "BYTES_PER_ELEMENT", Real.BYTES_PER_ELEMENT); }
		if (Real.isView) { define(Capped, "isView", Real.isView); }
		global[name] = Capped;
	}
	capConstructor("ArrayBuffer", 1);
	[["Int8Array", 1], ["Uint8Array", 1], ["Uint8ClampedArray", 1], ["Int16Array", 2], ["Uint16Array", 2],
	 ["Int32Array", 4], ["Uint32Array", 4], ["Float32Array", 4], ["Float64Array", 8],
	 ["BigInt64Array", 8], ["BigUint64Array", 8]].forEach(function (t) { capConstructor(t[0], t[1]); });
})(this);
`

// limitsProgram is compiled once; a goja Program is immutable and shared
// safely by every runtime.
var limitsProgram = goja.MustCompile("limits", fmt.Sprintf(limitsPrelude, maxStringLength, maxArrayLength, maxBufferBytes), false)

// installLimits caps the one-call allocators in a code hook's runtime. A
// failure is a host bug (the prelude is a constant), so it panics, as the
// sandbox's own prelude does.
func installLimits(rt *goja.Runtime) {
	if _, err := rt.RunProgram(limitsProgram); err != nil {
		panic(fmt.Sprintf("codehook: allocation-limit prelude failed: %v", err))
	}
}
