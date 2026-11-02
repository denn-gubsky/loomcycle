import {
  AdditiveBlending,
  Box3,
  BufferAttribute,
  BufferGeometry,
  LineSegments,
  Matrix4,
  NormalBlending,
  PerspectiveCamera,
  Points,
  Quaternion,
  Raycaster,
  Scene,
  ShaderMaterial,
  Sphere,
  Spherical,
  Vector2,
  Vector3,
  Vector4,
  WebGLRenderer,
} from "three";
import CameraControls from "camera-controls";
import { unit } from "../lib/hash";
import { chunkRgb, kindRgb, recency, type RGB } from "../lib/colors";
import type { Layout } from "../lib/layout";
import { computeLod, drawnLinks, starVisible, type Lod } from "../lib/lod";
import type { ProjectionFrame } from "../lib/projection";
import type { ModelEdge, UniverseModel } from "../lib/model";
import {
  FLAG_DEAD,
  FLAG_DIM,
  FLAG_FACT,
  FLAG_FORMATION,
  FLAG_HIT,
  FLAG_RAYS,
  FLAG_SELECTED,
  linkFragment,
  linkVertex,
  starFragment,
  starVertex,
  treeFragment,
  treeVertex,
} from "./shaders";

CameraControls.install({ THREE: { Vector2, Vector3, Vector4, Quaternion, Matrix4, Spherical, Box3, Sphere, Raycaster } });

// UniverseScene owns the WebGL side of the star map: one Points draw call for
// every star (chunks, then one per directory for collapsed formations), one
// LineSegments for containment inside constellations, one for the selection's
// typed links, and a pooled DOM layer for labels. React talks to it through a
// handful of setters; it never re-renders React.
//
// Frame policy: 60 fps while the camera moves or a crossfade runs; 30 fps while
// idle with animation on; no frames at all while idle with animation off, when
// the tab is hidden, or when the canvas is scrolled out of view.

export interface SceneCallbacks {
  onHover(star: number, x: number, y: number): void;
  onPick(star: number, additive: boolean): void;
  onDoublePick(star: number): void;
}

const FOV = 50;
const MAX_LABELS = 300;
const FADE_S = 0.3;
const IDLE_FRAME_MS = 1000 / 30;
const LOD_EVERY_MS = 120;
const LABEL_CELL_W = 24;
const MORPH_MS = 1000;

/** Which positions the stars take: their place in the Path tree, or their
 *  place in the embedding space. */
export interface CameraPose {
  position: [number, number, number];
  target: [number, number, number];
}

export type UniverseMode = "structure" | "meaning";
const LABEL_CELL_H = 7;

interface LabelItem {
  star: number;
  text: string;
  cls: string;
  /** World-space offset (formation titles sit above their sphere). */
  dy: number;
}

export class UniverseScene {
  private readonly container: HTMLElement;
  private readonly cb: SceneCallbacks;
  private readonly renderer: WebGLRenderer;
  private readonly scene = new Scene();
  private readonly camera = new PerspectiveCamera(FOV, 1, 0.01, 1e5);
  private readonly controls: CameraControls;
  private lastTick = 0;
  private readonly labelLayer: HTMLDivElement;
  private readonly labelPool: HTMLSpanElement[] = [];

  private model: UniverseModel | null = null;
  private layout: Layout | null = null;
  private lod: Lod | null = null;
  private starCount = 0;
  private prevChunkCount = 0;
  private framed: UniverseModel | null = null;

  private stars: Points | null = null;
  private tree: LineSegments | null = null;
  private links: LineSegments | null = null;
  private readonly starMat: ShaderMaterial;
  private readonly treeMat: ShaderMaterial;
  private readonly linkMat: ShaderMaterial;

  // pos is where the stars are NOW (picking, labels, framing read it); it is
  // posA (structure), posB (meaning) or a blend while the views morph.
  private pos = new Float32Array(0);
  private posA = new Float32Array(0);
  private posB = new Float32Array(0);
  private placedB = new Uint8Array(0);
  private frameB: ProjectionFrame | null = null;
  private mode: UniverseMode = "structure";
  private morph = 0;
  private morphFrom = 0;
  private morphTo = 0;
  private morphStart = 0;
  private marker: [number, number, number] | null = null;
  private readonly markerEl: HTMLDivElement;
  private vis = new Float32Array(0);
  private flags = new Float32Array(0);
  private size = new Float32Array(0);
  private baseFlags = new Float32Array(0);
  private treeChild = new Int32Array(0);
  private visibleStars = new Int32Array(0);

  private selected: number[] = [];
  private hits: number[] = [];
  private focus: Set<number> | null = null;
  private overlay: readonly ModelEdge[] = [];
  private labels: LabelItem[] = [];

  private animate = true;
  private reducedMotion = false;
  private shown = true;
  private dirty = true;
  private lodDirty = true;
  private lastLod = 0;
  private lastFrame = 0;
  private fadeUntil = 0;
  private time = 0;
  private raf = 0;
  private hoverAt: { x: number; y: number } | null = null;
  private down: { x: number; y: number; t: number } | null = null;
  private readonly cleanups: (() => void)[] = [];

  constructor(container: HTMLElement, cb: SceneCallbacks) {
    this.container = container;
    this.cb = cb;
    this.renderer = new WebGLRenderer({ antialias: true, alpha: true, powerPreference: "high-performance" });
    this.renderer.setPixelRatio(Math.min(2, window.devicePixelRatio || 1));
    this.renderer.setClearColor(0x000000, 0);
    const canvas = this.renderer.domElement;
    canvas.className = "lu-canvas";
    container.appendChild(canvas);
    this.labelLayer = document.createElement("div");
    this.labelLayer.className = "lu-labels";
    container.appendChild(this.labelLayer);
    this.markerEl = document.createElement("div");
    this.markerEl.className = "lu-marker";
    this.markerEl.textContent = "query";
    this.markerEl.style.display = "none";
    this.labelLayer.appendChild(this.markerEl);

    this.controls = new CameraControls(this.camera, canvas);
    this.controls.dollyToCursor = true;
    this.controls.smoothTime = 0.28;
    this.controls.draggingSmoothTime = 0.08;
    this.controls.minDistance = 0.3;
    this.controls.addEventListener("update", () => {
      this.dirty = true;
      this.lodDirty = true;
    });

    const uniforms = () => ({
      uTime: { value: 0 },
      uClock: { value: 0 },
      uSizeScale: { value: 1 },
      uPixelRatio: { value: this.renderer.getPixelRatio() },
      uLight: { value: 0 },
      uOpacity: { value: 0.34 },
      uFlow: { value: 1 },
      uMorph: { value: 0 },
    });
    this.starMat = new ShaderMaterial({
      uniforms: uniforms(),
      vertexShader: starVertex,
      fragmentShader: starFragment,
      transparent: true,
      depthTest: false,
      depthWrite: false,
      blending: AdditiveBlending,
    });
    this.treeMat = new ShaderMaterial({
      uniforms: uniforms(),
      vertexShader: treeVertex,
      fragmentShader: treeFragment,
      transparent: true,
      depthTest: false,
      depthWrite: false,
      blending: AdditiveBlending,
    });
    this.linkMat = new ShaderMaterial({
      uniforms: uniforms(),
      vertexShader: linkVertex,
      fragmentShader: linkFragment,
      transparent: true,
      depthTest: false,
      depthWrite: false,
      blending: AdditiveBlending,
    });

    this.bindEvents(canvas);
    this.refreshTheme();
    this.resize();
    this.raf = requestAnimationFrame(this.loop);
  }

  // ---- public API --------------------------------------------------------

  /** setData (re)builds every buffer for a new model + layout, with the
   *  meaning view's frame when the scope has a projection. The camera is framed
   *  on the whole sky the first time only. */
  setData(model: UniverseModel, layout: Layout, frameB: ProjectionFrame | null = null): void {
    // Frame the sky the first time a model has stars (a model is filled in
    // place page by page, so "first call" is not enough).
    const first = model.chunkCount > 0 && this.framed !== model;
    if (first) this.framed = model;
    if (model !== this.model) this.prevChunkCount = 0;
    this.model = model;
    this.layout = layout;
    this.lod = null;
    const n = model.chunkCount;
    const nd = layout.dirs.length;
    const S = n + nd;
    this.starCount = S;
    const pos = new Float32Array(S * 3);
    pos.set(layout.chunkPos);
    for (let d = 0; d < nd; d++) pos.set(layout.dirs[d].center, (n + d) * 3);
    const color = new Float32Array(S * 3);
    const seed = new Float32Array(S);
    const rec = new Float32Array(S);
    const size = new Float32Array(S);
    const flags = new Float32Array(S);
    const now = Date.now();
    const rootRgb: RGB[] = [];
    for (let c = 0; c < n; c++) {
      const rgb = chunkRgb(model, c);
      color.set(rgb, c * 3);
      seed[c] = unit(model.chunk.id[c]);
      rec[c] = recency(model.chunk.updatedAt[c], now);
      const depth = model.chunk.depth[c];
      size[c] = depth === 0 ? 0.5 : depth === 1 ? 0.28 : 0.2;
      const inv = model.chunk.invalidAt[c];
      const invMs = inv ? (inv > 1e14 ? inv / 1e6 : inv > 1e11 ? inv : inv * 1e3) : 0;
      let f = 0;
      if (model.chunk.retired[c] || (invMs > 0 && invMs <= now)) f |= FLAG_DEAD;
      if (depth === 0) {
        f |= FLAG_RAYS;
        rootRgb[model.chunk.doc[c]] = rgb;
      }
      if (model.chunk.isFact[c]) f |= FLAG_FACT;
      flags[c] = f;
    }
    for (let d = 0; d < nd; d++) {
      const dir = layout.dirs[d];
      const avg: RGB = [0, 0, 0];
      let k = 0;
      const walk = (i: number) => {
        for (const di of layout.dirs[i].docs) {
          const c = rootRgb[di];
          if (c) {
            avg[0] += c[0];
            avg[1] += c[1];
            avg[2] += c[2];
            k++;
          }
        }
        for (const ch of layout.dirs[i].children) if (k < 400) walk(ch);
      };
      walk(d);
      color.set(k ? [avg[0] / k, avg[1] / k, avg[2] / k] : [0.75, 0.8, 0.95], (n + d) * 3);
      seed[n + d] = unit(dir.path);
      size[n + d] = Math.max(0.6, dir.radius * 0.3);
      flags[n + d] = FLAG_FORMATION;
    }
    this.posA = pos;
    this.frameB = frameB;
    this.placedB = new Uint8Array(S);
    const posB = new Float32Array(pos);
    if (frameB) {
      for (let c = 0; c < n; c++) {
        if (!frameB.placed[c]) continue;
        this.placedB[c] = 1;
        posB[c * 3] = frameB.pos[c * 3];
        posB[c * 3 + 1] = frameB.pos[c * 3 + 1];
        posB[c * 3 + 2] = frameB.pos[c * 3 + 2];
      }
    }
    this.posB = posB;
    this.pos = new Float32Array(S * 3);
    this.blendCpu();
    this.size = size;
    this.baseFlags = flags;
    this.flags = new Float32Array(flags);
    // The model only ever appends chunks, so a chunk keeps its index across
    // pages: carry its fade state over instead of fading the sky in again.
    const vis = new Float32Array(S * 3);
    const oldChunks = Math.min(n, this.vis.length / 3, this.prevChunkCount);
    vis.set(this.vis.subarray(0, oldChunks * 3));
    this.vis = vis;
    this.prevChunkCount = n;

    const g = new BufferGeometry();
    g.setAttribute("position", new BufferAttribute(pos, 3));
    g.setAttribute("aPos2", new BufferAttribute(posB, 3));
    g.setAttribute("aColor", new BufferAttribute(color, 3));
    g.setAttribute("aSize", new BufferAttribute(this.size, 1));
    g.setAttribute("aSeed", new BufferAttribute(seed, 1));
    g.setAttribute("aRecency", new BufferAttribute(rec, 1));
    g.setAttribute("aFlags", new BufferAttribute(this.flags, 1));
    g.setAttribute("aVis", new BufferAttribute(this.vis, 3));
    g.boundingSphere = new Sphere(new Vector3(), layout.radius * 2 + 10);
    this.replace("stars", new Points(g, this.starMat));

    // Containment: one segment per chunk that has a parent.
    let segs = 0;
    for (let c = 0; c < n; c++) if (model.chunk.parent[c] >= 0) segs++;
    const tp = new Float32Array(segs * 6);
    const tp2 = new Float32Array(segs * 6);
    const tc = new Float32Array(segs * 6);
    const tv = new Float32Array(segs * 6);
    const child = new Int32Array(segs);
    let s = 0;
    for (let c = 0; c < n; c++) {
      const p = model.chunk.parent[c];
      if (p < 0) continue;
      tp.set(layout.chunkPos.subarray(p * 3, p * 3 + 3), s * 6);
      tp.set(layout.chunkPos.subarray(c * 3, c * 3 + 3), s * 6 + 3);
      tp2.set(posB.subarray(p * 3, p * 3 + 3), s * 6);
      tp2.set(posB.subarray(c * 3, c * 3 + 3), s * 6 + 3);
      tc.set(color.subarray(c * 3, c * 3 + 3), s * 6);
      tc.set(color.subarray(c * 3, c * 3 + 3), s * 6 + 3);
      child[s] = c;
      s++;
    }
    this.treeChild = child;
    const tg = new BufferGeometry();
    tg.setAttribute("position", new BufferAttribute(tp, 3));
    tg.setAttribute("aPos2", new BufferAttribute(tp2, 3));
    tg.setAttribute("aColor", new BufferAttribute(tc, 3));
    tg.setAttribute("aVis", new BufferAttribute(tv, 3));
    tg.boundingSphere = new Sphere(new Vector3(), layout.radius * 2 + 10);
    this.replace("tree", new LineSegments(tg, this.treeMat));

    this.camera.far = Math.max(1000, Math.max(layout.radius, frameB?.radius ?? 0) * 60);
    this.camera.updateProjectionMatrix();
    this.controls.maxDistance = Math.max(50, Math.max(layout.radius, frameB?.radius ?? 0) * 14);
    if (first) this.home(false);
    this.lodDirty = true;
    this.updateLod(true);
    this.dirty = true;
  }

  /** The selection (chunk indexes), the typed links to draw for it, and the
   *  neighbourhood to keep bright (null = no dimming). */
  setSelection(selected: number[], overlay: readonly ModelEdge[], focus: Set<number> | null): void {
    this.selected = selected;
    this.overlay = overlay;
    this.focus = focus;
    this.applyFlags();
    this.updateLod(true);
  }

  setHits(hits: number[]): void {
    this.hits = hits;
    this.applyFlags();
    this.updateLod(true);
  }

  /** setMode morphs the stars to the other view over a second, then frames it. */
  setMode(mode: UniverseMode): void {
    if (mode === this.mode) return;
    this.mode = mode;
    this.morphFrom = this.morph;
    this.morphTo = mode === "meaning" ? 1 : 0;
    this.morphStart = performance.now();
    this.lod = null;
    this.updateLod(true);
    this.home();
  }

  /** setMarker shows the search query's own place in the meaning view (scene
   *  coordinates), or hides it. */
  setMarker(xyz: [number, number, number] | null): void {
    this.marker = xyz;
    this.dirty = true;
  }

  setAnimate(on: boolean): void {
    this.animate = on;
    this.dirty = true;
  }

  /** refreshTheme re-reads the host's background to pick additive (dark sky)
   *  or normal (light, paper sky) blending. */
  refreshTheme(): void {
    const bg = getComputedStyle(this.container).getPropertyValue("--bg").trim();
    const m = /^#?([0-9a-f]{6})$/i.exec(bg);
    let light = false;
    if (m) {
      const v = parseInt(m[1], 16);
      const lum = (0.299 * ((v >> 16) & 255) + 0.587 * ((v >> 8) & 255) + 0.114 * (v & 255)) / 255;
      light = lum > 0.6;
    }
    for (const mat of [this.starMat, this.treeMat, this.linkMat]) {
      mat.uniforms.uLight.value = light ? 1 : 0;
      mat.blending = light ? NormalBlending : AdditiveBlending;
      mat.needsUpdate = true;
    }
    this.container.classList.toggle("lu-light", light);
    this.dirty = true;
  }

  /** flyTo moves the camera to a star: a chunk of an expanded document is
   *  framed closely, a document star frames its constellation, a formation
   *  star its whole formation. */
  flyTo(star: number): void {
    const sphere = this.sphereOf(star);
    if (sphere) void this.controls.fitToSphere(sphere, true);
  }

  /** frame fits the camera to several stars at once. */
  frame(stars: number[]): void {
    if (stars.length === 0) return;
    if (stars.length === 1) return this.flyTo(stars[0]);
    const box = new Box3();
    for (const s of stars) {
      const sp = this.sphereOf(s);
      if (sp) box.union(new Box3().setFromCenterAndSize(sp.center, new Vector3(1, 1, 1).multiplyScalar(sp.radius * 2)));
    }
    if (box.isEmpty()) return;
    const sphere = box.getBoundingSphere(new Sphere());
    void this.controls.fitToSphere(sphere, true);
  }

  /** The camera as a saved view keeps it: where it is and what it looks at. */
  getCamera(): CameraPose {
    const p = this.controls.getPosition(new Vector3());
    const t = this.controls.getTarget(new Vector3());
    return { position: [p.x, p.y, p.z], target: [t.x, t.y, t.z] };
  }

  /** setCamera restores a saved pose (a saved view's camera). */
  setCamera(pose: CameraPose, transition = true): void {
    const [px, py, pz] = pose.position;
    const [tx, ty, tz] = pose.target;
    void this.controls.setLookAt(px, py, pz, tx, ty, tz, transition);
    this.dirty = true;
  }

  home(transition = true): void {
    const r = this.mode === "meaning" && this.frameB ? this.frameB.radius * 1.05 : this.layout ? this.layout.radius : 10;
    void this.controls.fitToSphere(new Sphere(new Vector3(), Math.max(2, r)), transition);
  }

  /** isInView reports whether a star projects inside the canvas. */
  isInView(star: number): boolean {
    if (star < 0 || star >= this.starCount) return false;
    const v = new Vector3(this.pos[star * 3], this.pos[star * 3 + 1], this.pos[star * 3 + 2]).project(this.camera);
    return v.z < 1 && Math.abs(v.x) < 0.9 && Math.abs(v.y) < 0.9;
  }

  dispose(): void {
    cancelAnimationFrame(this.raf);
    for (const c of this.cleanups) c();
    this.controls.dispose();
    for (const o of [this.stars, this.tree, this.links]) o?.geometry.dispose();
    this.starMat.dispose();
    this.treeMat.dispose();
    this.linkMat.dispose();
    this.renderer.dispose();
    this.renderer.domElement.remove();
    this.labelLayer.remove();
  }

  // ---- internals ---------------------------------------------------------

  private replace(slot: "stars" | "tree" | "links", obj: Points | LineSegments | null) {
    const old = this[slot];
    if (old) {
      this.scene.remove(old);
      old.geometry.dispose();
    }
    if (obj) {
      obj.frustumCulled = false;
      this.scene.add(obj);
    }
    (this as unknown as Record<string, unknown>)[slot] = obj;
  }

  private sphereOf(star: number): Sphere | null {
    const m = this.model;
    const l = this.layout;
    if (!m || !l || star < 0 || star >= this.starCount) return null;
    if (star >= m.chunkCount) {
      const d = l.dirs[star - m.chunkCount];
      return new Sphere(new Vector3(...d.center), Math.max(1, d.radius));
    }
    const di = m.chunk.doc[star];
    // Frame where the star is going, not where a morph has it right now.
    const target = this.mode === "meaning" ? this.posB : this.posA;
    const center = new Vector3(target[star * 3], target[star * 3 + 1], target[star * 3 + 2]);
    if (this.mode === "meaning") return new Sphere(center, m.doc.root[di] === star ? 6 : 3);
    if (m.doc.root[di] === star) return new Sphere(center, Math.max(1.2, l.docRadius[di]));
    return new Sphere(center, 1.4);
  }

  private pinnedDocs(): Set<number> {
    const m = this.model;
    const out = new Set<number>();
    if (!m) return out;
    for (const s of [...this.selected, ...this.hits]) if (s >= 0 && s < m.chunkCount) out.add(m.chunk.doc[s]);
    return out;
  }

  private applyFlags() {
    const m = this.model;
    if (!m) return;
    const f = this.flags;
    f.set(this.baseFlags);
    if (this.focus) {
      for (let c = 0; c < m.chunkCount; c++) if (!this.focus.has(c)) f[c] += FLAG_DIM;
    }
    for (const s of this.hits) if (s >= 0 && s < m.chunkCount && (f[s] & FLAG_HIT) === 0) f[s] += FLAG_HIT;
    for (const s of this.selected) if (s >= 0 && s < m.chunkCount && (f[s] & FLAG_SELECTED) === 0) f[s] += FLAG_SELECTED;
    // A focused star is never dimmed, whatever the neighbourhood said.
    for (const s of [...this.hits, ...this.selected]) if (s >= 0 && s < m.chunkCount && (f[s] & FLAG_DIM) !== 0) f[s] -= FLAG_DIM;
    const attr = this.stars?.geometry.getAttribute("aFlags") as BufferAttribute | undefined;
    if (attr) attr.needsUpdate = true;
    this.dirty = true;
  }

  private pxPerUnit(): number {
    const h = this.container.clientHeight || 1;
    return h / (2 * Math.tan((FOV * Math.PI) / 360));
  }

  private updateLod(force: boolean) {
    const m = this.model;
    const l = this.layout;
    if (!m || !l || !this.stars) return;
    const now = performance.now();
    if (!force && now - this.lastLod < LOD_EVERY_MS) return;
    this.lastLod = now;
    this.lodDirty = false;
    const cam = this.camera.position;
    const view = { cam: [cam.x, cam.y, cam.z] as [number, number, number], pxPerUnit: this.pxPerUnit() };
    // The meaning view has no formations to collapse: every placed chunk is
    // its own star, and a link is drawn between the chunks themselves.
    const meaning = this.mode === "meaning";
    const next: Lod = meaning
      ? {
          dirCollapsed: new Uint8Array(l.dirs.length),
          dirTop: new Int32Array(l.dirs.length).fill(-1),
          docExpanded: new Uint8Array(m.docCount).fill(1),
        }
      : computeLod(m, l, view, this.lod, this.pinnedDocs());
    this.lod = next;
    const clock = now / 1000;
    const vis = this.vis;
    const visible: number[] = [];
    let changed = false;
    for (let s = 0; s < this.starCount; s++) {
      const target = (meaning ? s < m.chunkCount && this.placedB[s] === 1 : starVisible(m, l, next, s)) ? 1 : 0;
      if (target) visible.push(s);
      const to = vis[s * 3 + 1];
      if (to === target) continue;
      const from = vis[s * 3];
      const t = Math.min(1, Math.max(0, (clock - vis[s * 3 + 2]) / FADE_S));
      vis[s * 3] = from + (to - from) * t;
      vis[s * 3 + 1] = target;
      vis[s * 3 + 2] = clock;
      changed = true;
    }
    // A document star stands for its whole constellation while collapsed.
    for (let di = 0; di < m.docCount; di++) {
      const r = m.doc.root[di];
      if (r < 0) continue;
      this.size[r] = next.docExpanded[di] ? 0.5 : Math.max(0.5, l.docRadius[di] * 0.45);
    }
    this.visibleStars = Int32Array.from(visible);
    const g = this.stars.geometry;
    (g.getAttribute("aSize") as BufferAttribute).needsUpdate = true;
    if (changed) {
      (g.getAttribute("aVis") as BufferAttribute).needsUpdate = true;
      this.fadeUntil = now + FADE_S * 1000 + 50;
      if (this.tree) {
        const tv = this.tree.geometry.getAttribute("aVis") as BufferAttribute;
        const arr = tv.array as Float32Array;
        for (let i = 0; i < this.treeChild.length; i++) {
          const c = this.treeChild[i];
          for (const k of [0, 3]) {
            arr[i * 6 + k] = vis[c * 3];
            arr[i * 6 + k + 1] = vis[c * 3 + 1];
            arr[i * 6 + k + 2] = vis[c * 3 + 2];
          }
        }
        tv.needsUpdate = true;
      }
    }
    this.rebuildLinks();
    this.pickLabels(view.pxPerUnit);
    this.dirty = true;
  }

  private rebuildLinks() {
    const m = this.model;
    const l = this.layout;
    if (!m || !l || !this.lod || this.overlay.length === 0) {
      this.replace("links", null);
      return;
    }
    const drawn = drawnLinks(m, l, this.lod, this.overlay).filter(
      (x) => this.mode !== "meaning" || (this.placedB[x.a] === 1 && this.placedB[x.b] === 1),
    );
    const n = drawn.length;
    const p = new Float32Array(n * 6);
    const p2 = new Float32Array(n * 6);
    const t = new Float32Array(n * 2);
    const len = new Float32Array(n * 2);
    const col = new Float32Array(n * 6);
    const w = new Float32Array(n * 2);
    const prop = new Float32Array(n * 2);
    const strength = new Float32Array(n * 2);
    const expired = new Float32Array(n * 2);
    drawn.forEach((x, i) => {
      p.set(this.posA.subarray(x.a * 3, x.a * 3 + 3), i * 6);
      p.set(this.posA.subarray(x.b * 3, x.b * 3 + 3), i * 6 + 3);
      p2.set(this.posB.subarray(x.a * 3, x.a * 3 + 3), i * 6);
      p2.set(this.posB.subarray(x.b * 3, x.b * 3 + 3), i * 6 + 3);
      const q = this.mode === "meaning" ? p2 : p;
      const L = Math.hypot(q[i * 6] - q[i * 6 + 3], q[i * 6 + 1] - q[i * 6 + 4], q[i * 6 + 2] - q[i * 6 + 5]);
      t[i * 2] = 0;
      t[i * 2 + 1] = 1;
      len[i * 2] = len[i * 2 + 1] = L;
      const rgb = kindRgb(x.kind);
      col.set(rgb, i * 6);
      col.set(rgb, i * 6 + 3);
      w[i * 2] = w[i * 2 + 1] = x.count;
      prop[i * 2] = prop[i * 2 + 1] = x.proposed ? 1 : 0;
      strength[i * 2] = strength[i * 2 + 1] = x.confidence;
      expired[i * 2] = expired[i * 2 + 1] = x.expired ? 1 : 0;
    });
    const g = new BufferGeometry();
    g.setAttribute("position", new BufferAttribute(p, 3));
    g.setAttribute("aPos2", new BufferAttribute(p2, 3));
    g.setAttribute("aT", new BufferAttribute(t, 1));
    g.setAttribute("aLen", new BufferAttribute(len, 1));
    g.setAttribute("aColor", new BufferAttribute(col, 3));
    g.setAttribute("aWeight", new BufferAttribute(w, 1));
    g.setAttribute("aProposed", new BufferAttribute(prop, 1));
    g.setAttribute("aStrength", new BufferAttribute(strength, 1));
    g.setAttribute("aExpired", new BufferAttribute(expired, 1));
    this.replace("links", new LineSegments(g, this.linkMat));
  }

  private projectedPx(x: number, y: number, z: number, r: number, ppu: number): number {
    const c = this.camera.position;
    const d = Math.hypot(x - c.x, y - c.y, z - c.z);
    return d <= r ? Infinity : (r * ppu) / d;
  }

  // pickLabels chooses at most MAX_LABELS labels: the selection first, then
  // formations, documents and (for documents close enough) section headings,
  // each ranked by how large it is on screen.
  private pickLabels(ppu: number) {
    const m = this.model;
    const l = this.layout;
    const lod = this.lod;
    if (!m || !l || !lod) return;
    const cand: (LabelItem & { pri: number })[] = [];
    const n = m.chunkCount;
    if (this.mode === "meaning") {
      // No formations here: documents and, close up, their sections, ranked
      // by how near they are (a nominal 4-unit radius on screen).
      for (let di = 0; di < m.docCount; di++) {
        const r = m.doc.root[di];
        if (r < 0 || !this.placedB[r]) continue;
        const px = this.projectedPx(this.posB[r * 3], this.posB[r * 3 + 1], this.posB[r * 3 + 2], 4, ppu);
        if (px >= 14) cand.push({ star: r, text: trim(m.doc.title[di] || m.doc.id[di]), cls: "lu-label--doc", dy: 0, pri: px });
      }
      for (let c = 0; c < n; c++) {
        if (!this.placedB[c] || m.chunk.depth[c] === 0 || m.chunk.depth[c] > 2 || !m.chunk.title[c]) continue;
        const px = this.projectedPx(this.posB[c * 3], this.posB[c * 3 + 1], this.posB[c * 3 + 2], 4, ppu);
        if (px >= 120) cand.push({ star: c, text: trim(m.chunk.title[c]), cls: "lu-label--chunk", dy: 0, pri: px / 3 });
      }
    }
    for (let d = 1; d < l.dirs.length && this.mode === "structure"; d++) {
      const dir = l.dirs[d];
      if (lod.dirTop[d] === d) {
        const px = this.projectedPx(dir.center[0], dir.center[1], dir.center[2], dir.radius, ppu);
        cand.push({ star: n + d, text: `${dir.name} · ${dir.docTotal}`, cls: "lu-label--dir", dy: 0, pri: px + 20 });
      } else if (lod.dirTop[d] < 0 && dir.docTotal > 1) {
        const px = this.projectedPx(dir.center[0], dir.center[1], dir.center[2], dir.radius, ppu);
        if (px > 30 && px < 1400) cand.push({ star: n + d, text: dir.path, cls: "lu-label--formation", dy: dir.radius * 0.92, pri: 15 + px / 20 });
      }
    }
    for (let di = 0; di < m.docCount && this.mode === "structure"; di++) {
      if (lod.dirTop[l.docDir[di]] >= 0) continue;
      const r = m.doc.root[di];
      if (r < 0) continue;
      const px = this.projectedPx(l.docCenter[di * 3], l.docCenter[di * 3 + 1], l.docCenter[di * 3 + 2], l.docRadius[di], ppu);
      if (px >= 14) cand.push({ star: r, text: trim(m.doc.title[di] || m.doc.id[di]), cls: "lu-label--doc", dy: 0, pri: px });
      if (lod.docExpanded[di] && px >= 220) {
        const start = m.doc.chunkStart[di];
        for (let c = start; c < start + m.doc.chunkCount[di]; c++) {
          const depth = m.chunk.depth[c];
          if (depth === 0 || depth > (px >= 520 ? 2 : 1) || !m.chunk.title[c]) continue;
          cand.push({ star: c, text: trim(m.chunk.title[c]), cls: "lu-label--chunk", dy: 0, pri: px / (depth + 1) });
        }
      }
    }
    cand.sort((a, b) => b.pri - a.pri);
    // Keyed by star, except a formation title, which may share its star with
    // the formation's own collapsed label.
    const chosen = new Map<string, LabelItem>();
    for (const s of this.selected) {
      if (s < 0 || s >= n) continue;
      const di = m.chunk.doc[s];
      const text = m.doc.root[di] === s ? m.doc.title[di] : m.chunk.title[s];
      chosen.set(`s${s}`, { star: s, text: trim(text || m.chunk.id[s]), cls: "lu-label--selected", dy: 0 });
    }
    for (const c of cand) {
      if (chosen.size >= MAX_LABELS) break;
      const key = c.cls === "lu-label--formation" ? `f${c.star}` : `s${c.star}`;
      if (!chosen.has(key)) chosen.set(key, c);
    }
    this.labels = [...chosen.values()];
  }

  private placeLabels() {
    const w = this.container.clientWidth;
    const h = this.container.clientHeight;
    const v = new Vector3();
    // Labels arrive in priority order (selection first); one that would overlap
    // a label already placed this frame is skipped, so a crowded formation shows
    // its largest names legibly instead of a pile of text. Occupancy is a coarse
    // grid of LABEL_CELL_W × LABEL_CELL_H cells.
    const taken = new Set<number>();
    const cols = Math.ceil(w / LABEL_CELL_W) + 1;
    let i = 0;
    for (const lab of this.labels) {
      const s = lab.star;
      v.set(this.pos[s * 3], this.pos[s * 3 + 1] + lab.dy, this.pos[s * 3 + 2]).project(this.camera);
      if (v.z > 1 || v.x < -1.1 || v.x > 1.1 || v.y < -1.1 || v.y > 1.1) continue;
      const x = ((v.x + 1) / 2) * w;
      const y = ((1 - v.y) / 2) * h;
      // Formation titles are centred above their point; the rest start 8px right.
      const left = lab.cls === "lu-label--formation" ? x - lab.text.length * 3 : x + 8;
      const top = lab.cls === "lu-label--formation" ? y - 14 : y + 2;
      const c0 = Math.max(0, Math.floor(left / LABEL_CELL_W));
      const c1 = Math.max(0, Math.floor((left + lab.text.length * 6 + 4) / LABEL_CELL_W));
      const r0 = Math.max(0, Math.floor(top / LABEL_CELL_H));
      const r1 = Math.max(0, Math.floor((top + 12) / LABEL_CELL_H));
      let free = true;
      for (let r = r0; r <= r1 && free; r++) for (let c = c0; c <= c1; c++) if (taken.has(r * cols + c)) free = false;
      if (!free && lab.cls !== "lu-label--selected") continue;
      for (let r = r0; r <= r1; r++) for (let c = c0; c <= c1; c++) taken.add(r * cols + c);
      let el = this.labelPool[i];
      if (!el) {
        el = document.createElement("span");
        this.labelLayer.appendChild(el);
        this.labelPool.push(el);
      }
      if (el.textContent !== lab.text) el.textContent = lab.text;
      const cls = `lu-label ${lab.cls}`;
      if (el.className !== cls) el.className = cls;
      el.style.transform = `translate(${x}px, ${y}px)`;
      el.style.display = "";
      i++;
    }
    for (; i < this.labelPool.length; i++) this.labelPool[i].style.display = "none";
    // The query marker belongs to the meaning view only.
    const mk = this.marker;
    if (mk && this.morph > 0.5) {
      v.set(mk[0], mk[1], mk[2]).project(this.camera);
      if (v.z <= 1) {
        this.markerEl.style.transform = `translate(${((v.x + 1) / 2) * w}px, ${((1 - v.y) / 2) * h}px)`;
        this.markerEl.style.display = "";
        return;
      }
    }
    this.markerEl.style.display = "none";
  }

  // blendCpu sets the CPU positions to the current morph between the views.
  private blendCpu() {
    const a = this.posA;
    const b = this.posB;
    const t = this.morph;
    const out = this.pos;
    if (t <= 0) out.set(a);
    else if (t >= 1) out.set(b);
    else for (let i = 0; i < out.length; i++) out[i] = a[i] + (b[i] - a[i]) * t;
  }

  private pick(x: number, y: number): number {
    const w = this.container.clientWidth;
    const h = this.container.clientHeight;
    const mvp = new Matrix4().multiplyMatrices(this.camera.projectionMatrix, this.camera.matrixWorldInverse);
    const e = mvp.elements;
    const ppu = this.pxPerUnit();
    let best = -1;
    let bestD = Infinity;
    const vs = this.visibleStars;
    for (let i = 0; i < vs.length; i++) {
      const s = vs[i];
      const px = this.pos[s * 3];
      const py = this.pos[s * 3 + 1];
      const pz = this.pos[s * 3 + 2];
      const cw = e[3] * px + e[7] * py + e[11] * pz + e[15];
      if (cw <= 0) continue;
      const sx = (((e[0] * px + e[4] * py + e[8] * pz + e[12]) / cw + 1) / 2) * w;
      const sy = ((1 - (e[1] * px + e[5] * py + e[9] * pz + e[13]) / cw) / 2) * h;
      const d = Math.hypot(sx - x, sy - y);
      const r = Math.max(7, Math.min(40, (this.size[s] * ppu) / cw) * 0.6);
      // Prefer the nearest to the cursor, then the nearest to the camera.
      const score = d + cw * 1e-6;
      if (d <= r && score < bestD) {
        bestD = score;
        best = s;
      }
    }
    return best;
  }

  private bindEvents(canvas: HTMLCanvasElement) {
    const local = (e: PointerEvent | MouseEvent) => {
      const r = canvas.getBoundingClientRect();
      return { x: e.clientX - r.left, y: e.clientY - r.top };
    };
    const onMove = (e: PointerEvent) => {
      this.hoverAt = local(e);
    };
    const onLeave = () => {
      this.hoverAt = null;
      this.cb.onHover(-1, 0, 0);
    };
    const onDown = (e: PointerEvent) => {
      this.down = { ...local(e), t: performance.now() };
    };
    const onUp = (e: PointerEvent) => {
      const d = this.down;
      this.down = null;
      if (!d || e.button !== 0) return;
      const p = local(e);
      if (Math.hypot(p.x - d.x, p.y - d.y) > 4 || performance.now() - d.t > 500) return;
      const s = this.pick(p.x, p.y);
      if (s >= 0) this.cb.onPick(s, e.shiftKey || e.metaKey || e.ctrlKey);
    };
    const onDbl = (e: MouseEvent) => {
      const p = local(e);
      const s = this.pick(p.x, p.y);
      if (s >= 0) this.cb.onDoublePick(s);
    };
    canvas.addEventListener("pointermove", onMove);
    canvas.addEventListener("pointerleave", onLeave);
    canvas.addEventListener("pointerdown", onDown);
    canvas.addEventListener("pointerup", onUp);
    canvas.addEventListener("dblclick", onDbl);
    this.cleanups.push(() => {
      canvas.removeEventListener("pointermove", onMove);
      canvas.removeEventListener("pointerleave", onLeave);
      canvas.removeEventListener("pointerdown", onDown);
      canvas.removeEventListener("pointerup", onUp);
      canvas.removeEventListener("dblclick", onDbl);
    });

    const ro = new ResizeObserver(() => this.resize());
    ro.observe(this.container);
    this.cleanups.push(() => ro.disconnect());
    const io = new IntersectionObserver((es) => {
      this.shown = es.some((x) => x.isIntersecting);
      this.dirty = true;
    });
    io.observe(this.container);
    this.cleanups.push(() => io.disconnect());
    const mq = window.matchMedia?.("(prefers-reduced-motion: reduce)");
    if (mq) {
      this.reducedMotion = mq.matches;
      const onMq = () => {
        this.reducedMotion = mq.matches;
        this.dirty = true;
      };
      mq.addEventListener("change", onMq);
      this.cleanups.push(() => mq.removeEventListener("change", onMq));
    }
  }

  private resize() {
    const w = Math.max(1, this.container.clientWidth);
    const h = Math.max(1, this.container.clientHeight);
    this.renderer.setSize(w, h, false);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
    this.lodDirty = true;
    this.dirty = true;
  }

  private readonly loop = (nowMs: number) => {
    this.raf = requestAnimationFrame(this.loop);
    const delta = this.lastTick ? Math.min(0.25, (nowMs - this.lastTick) / 1000) : 0;
    this.lastTick = nowMs;
    if (!this.shown || document.hidden) return;
    const moved = this.controls.update(delta);
    const animating = this.animate && !this.reducedMotion;
    // Animation time runs with the wall clock while animating, rendered frame
    // or not, so the idle frame cap does not slow the twinkle down.
    if (animating) this.time += Math.min(0.1, delta);
    const fading = nowMs < this.fadeUntil;
    let morphing = false;
    if (this.morph !== this.morphTo) {
      const t = Math.min(1, (nowMs - this.morphStart) / MORPH_MS);
      const ease = t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;
      this.morph = t >= 1 ? this.morphTo : this.morphFrom + (this.morphTo - this.morphFrom) * ease;
      this.blendCpu();
      morphing = true;
    }
    if (this.lodDirty) this.updateLod(!moved);
    const urgent = this.dirty || moved || fading || morphing;
    if (!urgent) {
      if (!animating || nowMs - this.lastFrame < IDLE_FRAME_MS) {
        this.handleHover();
        return;
      }
    }
    const ppu = this.pxPerUnit();
    for (const mat of [this.starMat, this.treeMat, this.linkMat]) {
      mat.uniforms.uTime.value = this.time;
      mat.uniforms.uClock.value = nowMs / 1000;
      mat.uniforms.uSizeScale.value = ppu;
      mat.uniforms.uFlow.value = animating ? 1 : 0;
      mat.uniforms.uMorph.value = this.morph;
    }
    this.renderer.render(this.scene, this.camera);
    this.placeLabels();
    this.handleHover();
    this.lastFrame = nowMs;
    this.dirty = false;
  };

  private handleHover() {
    if (!this.hoverAt || this.down || this.controls.currentAction !== 0) return;
    const { x, y } = this.hoverAt;
    this.hoverAt = null;
    this.cb.onHover(this.pick(x, y), x, y);
  }
}

function trim(s: string): string {
  return s.length > 48 ? s.slice(0, 46) + "…" : s;
}
