// The star and link shaders. All animation lives here, driven by two uniforms:
// uTime advances only while animation is on (frozen for reduced motion or the
// toggle), uClock always advances (level-of-detail crossfades must finish even
// when the sky is still).
//
// aFlags bits: 1 rays, 2 selected, 4 search hit, 8 dead (retired / invalid),
// 16 dimmed (outside the focused neighbourhood), 32 formation star, 64 fact.
//
// Every buffer carries two positions — `position` (the structure view) and
// `aPos2` (the meaning view, the embedding projection) — and uMorph blends
// them, so switching views moves the same stars instead of swapping scenes.

export const FLAG_RAYS = 1;
export const FLAG_SELECTED = 2;
export const FLAG_HIT = 4;
export const FLAG_DEAD = 8;
export const FLAG_DIM = 16;
export const FLAG_FORMATION = 32;
export const FLAG_FACT = 64;

export const starVertex = /* glsl */ `
attribute vec3 aColor;
attribute float aSize;
attribute float aSeed;
attribute float aRecency;
attribute float aFlags;
attribute vec3 aVis; // from, to, fade start (uClock seconds)
attribute vec3 aPos2;
uniform float uMorph;
uniform float uTime;
uniform float uClock;
uniform float uSizeScale;
uniform float uPixelRatio;
uniform float uLight;
varying vec3 vColor;
varying float vAlpha;
varying float vSprite;
varying float vSeed;
varying float vTwinkle;
varying float vRays;
varying float vSelected;
varying float vHit;
varying float vFormation;
varying float vFact;

float bit(float flags, float b) { return mod(floor(flags / b), 2.0); }

void main() {
  float fade = clamp((uClock - aVis.z) / 0.3, 0.0, 1.0);
  float vis = mix(aVis.x, aVis.y, smoothstep(0.0, 1.0, fade));
  float dead = bit(aFlags, 8.0);
  float dim = bit(aFlags, 16.0);
  vRays = max(bit(aFlags, 1.0), max(bit(aFlags, 2.0), bit(aFlags, 4.0)));
  vSelected = bit(aFlags, 2.0);
  vHit = bit(aFlags, 4.0);
  vFormation = bit(aFlags, 32.0);
  vFact = bit(aFlags, 64.0);
  // Twinkle: every star on its own phase; what changed recently sparkles
  // faster and deeper, calming over a day. A dead star does not twinkle.
  float w = mix(0.8, 2.5, aRecency);
  float amp = mix(0.06, 0.25, aRecency) * (1.0 - dead);
  vTwinkle = 1.0 + amp * sin(uTime * w + aSeed * 6.2831853);
  vSeed = aSeed;
  vec4 mv = modelViewMatrix * vec4(mix(position, aPos2, uMorph), 1.0);
  float sprite = vRays > 0.5 ? 3.4 : (vFormation > 0.5 ? 2.0 : 1.0);
  float px = aSize * uSizeScale / max(0.0001, -mv.z);
  // A floor per kind keeps far formations and document stars findable.
  float floorPx = vFormation > 0.5 ? 10.0 : (bit(aFlags, 1.0) > 0.5 ? 3.2 : 1.8);
  float size = clamp(px * vTwinkle, floorPx, 44.0) * sprite;
  gl_PointSize = vis > 0.002 ? size * uPixelRatio : 0.0;
  vSprite = 1.0 / sprite;
  vAlpha = vis * (dead > 0.5 ? 0.45 : 1.0) * (dim > 0.5 ? 0.22 : 1.0);
  vec3 c = aColor * (0.85 + 0.3 * aRecency);
  if (dead > 0.5) c = mix(vec3(dot(aColor, vec3(0.3, 0.59, 0.11))), aColor, 0.25) * vec3(1.0, 0.82, 0.82);
  if (uLight > 0.5) c *= 0.72;
  vColor = c;
  gl_Position = projectionMatrix * mv;
}
`;

export const starFragment = /* glsl */ `
uniform float uTime;
uniform float uLight;
varying vec3 vColor;
varying float vAlpha;
varying float vSprite;
varying float vSeed;
varying float vTwinkle;
varying float vRays;
varying float vSelected;
varying float vHit;
varying float vFormation;
varying float vFact;

vec2 rot(vec2 p, float a) { float c = cos(a), s = sin(a); return vec2(c * p.x - s * p.y, s * p.x + c * p.y); }

void main() {
  vec2 uv = gl_PointCoord * 2.0 - 1.0;
  float r = length(uv);
  if (r > 1.0) discard;
  // A fact is a diamond, so the memory tier reads apart from document prose.
  float d = vFact > 0.5 ? (abs(uv.x) + abs(uv.y)) / vSprite : r / vSprite;
  float a;
  if (vFormation > 0.5) {
    // A collapsed formation: a soft nebula glow rather than a hard point.
    a = exp(-d * d * 1.6) * 0.75 + exp(-d * 1.4) * 0.25;
  } else {
    a = exp(-d * d * 7.0) + exp(-d * 2.4) * 0.3;
  }
  if (vRays > 0.5) {
    // Diffraction rays: four main spikes and a fainter pair, turning slowly.
    vec2 p = rot(uv, uTime * 0.05 + vSeed * 6.2831853);
    float len = (0.55 + 0.45 * vTwinkle) * (1.0 - r);
    float spikes = exp(-abs(p.y) * 90.0) + exp(-abs(p.x) * 90.0);
    vec2 q = rot(p, 0.785398);
    float minor = exp(-abs(q.y) * 140.0) * 0.35;
    a += (spikes + minor) * len * len * 0.9;
  }
  if (vSelected > 0.5) {
    // Selection pulse: a thin ring expanding and fading every two seconds.
    float t = fract(uTime * 0.5 + vSeed);
    float rp = mix(0.3, 0.95, t);
    a += smoothstep(0.05, 0.0, abs(r - rp)) * (1.0 - t) * 0.9;
    a += smoothstep(0.04, 0.0, abs(r - 0.28)) * 0.5;
  } else if (vHit > 0.5) {
    a += smoothstep(0.04, 0.0, abs(r - 0.3)) * 0.55;
  }
  a = clamp(a, 0.0, 1.6) * vAlpha;
  if (a < 0.004) discard;
  if (uLight > 0.5) {
    gl_FragColor = vec4(vColor, min(1.0, a));
  } else {
    gl_FragColor = vec4(vColor * a, a);
  }
}
`;

// Containment lines (parent → child inside a constellation): faint, permanent,
// fading with the child star.
export const treeVertex = /* glsl */ `
attribute vec3 aVis;
attribute vec3 aColor;
attribute vec3 aPos2;
uniform float uClock;
uniform float uMorph;
varying float vAlpha;
varying vec3 vColor;
void main() {
  float fade = clamp((uClock - aVis.z) / 0.3, 0.0, 1.0);
  // Containment belongs to the structure view: it fades out on the way to
  // the meaning view, where a chunk sits by what it says, not where it is.
  vAlpha = mix(aVis.x, aVis.y, smoothstep(0.0, 1.0, fade)) * (1.0 - smoothstep(0.0, 0.6, uMorph));
  vColor = aColor;
  gl_Position = projectionMatrix * modelViewMatrix * vec4(mix(position, aPos2, uMorph), 1.0);
}
`;

export const treeFragment = /* glsl */ `
uniform float uLight;
uniform float uOpacity;
varying float vAlpha;
varying vec3 vColor;
void main() {
  float a = vAlpha * uOpacity;
  if (a < 0.003) discard;
  gl_FragColor = uLight > 0.5 ? vec4(vColor * 0.6, a) : vec4(vColor * a, a);
}
`;

// Typed links of the selection: colour by kind, weight by how many edges merged
// into the line, and a bright band flowing from source to target (direction).
// Proposed statements draw dashed and flow slower.
export const linkVertex = /* glsl */ `
attribute float aT;
attribute float aLen;
attribute vec3 aPos2;
uniform float uMorph;
attribute vec3 aColor;
attribute float aWeight;
attribute float aProposed;
varying float vS;
varying vec3 vColor;
varying float vWeight;
varying float vProposed;
void main() {
  vS = aT * aLen;
  vColor = aColor;
  vWeight = aWeight;
  vProposed = aProposed;
  gl_Position = projectionMatrix * modelViewMatrix * vec4(mix(position, aPos2, uMorph), 1.0);
}
`;

export const linkFragment = /* glsl */ `
uniform float uTime;
uniform float uLight;
uniform float uFlow;
varying float vS;
varying vec3 vColor;
varying float vWeight;
varying float vProposed;
void main() {
  if (vProposed > 0.5 && fract(vS * 1.5) > 0.55) discard;
  float speed = vProposed > 0.5 ? 0.35 : 0.8;
  float band = smoothstep(0.75, 1.0, fract(vS * 0.35 - uTime * speed)) * uFlow;
  float a = clamp(0.45 + 0.18 * log2(vWeight) + band * 0.6, 0.0, 1.0);
  gl_FragColor = uLight > 0.5 ? vec4(vColor * 0.75, a) : vec4(vColor * a, a);
}
`;
