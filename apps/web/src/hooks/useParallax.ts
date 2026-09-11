import { useEffect, type RefObject } from "react";

const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

// Scroll effects for the landing page: eased parallax layers ([data-speed]),
// reveal-on-scroll (.reveal), a fade-out for [data-fade] and a progress var.
// Everything degrades to a static page under prefers-reduced-motion.
export function useParallax(root: RefObject<HTMLElement>) {
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const reduce = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
    const cleanups: Array<() => void> = [];

    const reveals = Array.from(el.querySelectorAll<HTMLElement>(".reveal"));
    if (reduce || typeof IntersectionObserver === "undefined") {
      reveals.forEach((r) => r.classList.add("in"));
    } else {
      const io = new IntersectionObserver(
        (entries) => {
          for (const e of entries) {
            if (e.isIntersecting) {
              e.target.classList.add("in");
              io.unobserve(e.target);
            }
          }
        },
        { threshold: 0.12, rootMargin: "0px 0px -6% 0px" },
      );
      reveals.forEach((r) => io.observe(r));
      cleanups.push(() => io.disconnect());
    }

    if (reduce) return () => cleanups.forEach((c) => c());

    document.documentElement.classList.add("lp-smooth");
    cleanups.push(() => document.documentElement.classList.remove("lp-smooth"));

    const layers = Array.from(el.querySelectorAll<HTMLElement>("[data-speed]")).map((node) => ({
      node,
      speed: parseFloat(node.dataset.speed ?? "0") || 0,
      mid: 0,
    }));
    const fades = Array.from(el.querySelectorAll<HTMLElement>("[data-fade]"));

    // Document-space midpoint of each layer's parent. offsetTop ignores
    // transforms, so entrance animations cannot skew the measurement.
    const measure = () => {
      for (const l of layers) {
        const host = l.node.parentElement;
        if (!host) continue;
        let top = 0;
        for (let n: HTMLElement | null = host; n; n = n.offsetParent as HTMLElement | null) top += n.offsetTop;
        l.mid = top + host.offsetHeight / 2;
      }
    };

    let cur = window.scrollY;
    let raf = 0;
    const apply = () => {
      const vh = window.innerHeight;
      const center = cur + vh / 2;
      for (const l of layers) {
        l.node.style.transform = `translate3d(0, ${((center - l.mid) * l.speed).toFixed(2)}px, 0)`;
      }
      const o = clamp(1 - cur / (vh * 0.75), 0, 1).toFixed(3);
      for (const f of fades) f.style.opacity = o;
      const max = document.documentElement.scrollHeight - vh;
      el.style.setProperty("--progress", max > 0 ? String(clamp(cur / max, 0, 1)) : "0");
    };
    const tick = () => {
      const target = window.scrollY;
      cur += (target - cur) * 0.14;
      if (Math.abs(target - cur) < 0.1) cur = target;
      apply();
      raf = cur === target ? 0 : requestAnimationFrame(tick);
    };
    const kick = () => {
      if (!raf) raf = requestAnimationFrame(tick);
    };
    const onResize = () => {
      measure();
      kick();
    };

    measure();
    apply();
    window.addEventListener("scroll", kick, { passive: true });
    window.addEventListener("resize", onResize);
    window.addEventListener("load", onResize);
    // Fonts and images shift layout after first paint.
    const ro = typeof ResizeObserver !== "undefined" ? new ResizeObserver(onResize) : null;
    ro?.observe(el);
    cleanups.push(() => {
      window.removeEventListener("scroll", kick);
      window.removeEventListener("resize", onResize);
      window.removeEventListener("load", onResize);
      ro?.disconnect();
      if (raf) cancelAnimationFrame(raf);
    });

    return () => cleanups.forEach((c) => c());
  }, [root]);
}
