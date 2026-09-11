import { useEffect, type MouseEvent } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useAuth } from "../state/auth";
import { Logo } from "./Logo";
import "../pages/landing.css";

export function scrollToId(id: string) {
  const reduce = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
  document.getElementById(id)?.scrollIntoView({ behavior: reduce ? "auto" : "smooth" });
}

const sections: [string, string][] = [
  ["features", "Features"],
  ["how", "How it works"],
  ["reliability", "Reliability"],
  ["numbers", "Numbers"],
];

// Top bar shared by the landing page and the login/register page.
export function SiteNav() {
  const { session } = useAuth();
  const { pathname } = useLocation();
  const nav = useNavigate();

  useEffect(() => {
    const el = document.querySelector<HTMLElement>(".lp-nav");
    const onScroll = () => el?.classList.toggle("scrolled", window.scrollY > 8);
    onScroll();
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);

  const go = (id: string) => (e: MouseEvent) => {
    e.preventDefault();
    if (pathname === "/") scrollToId(id);
    else nav(`/#${id}`);
  };

  return (
    <header className="lp-nav">
      <div className="lp-wrap lp-nav-in">
        <Link to="/" className="lp-brand">
          <Logo />
          Synapse
        </Link>
        <nav className="lp-links" aria-label="Sections">
          {sections.map(([id, label]) => (
            <a key={id} href={`/#${id}`} onClick={go(id)}>
              {label}
            </a>
          ))}
        </nav>
        <div className="lp-nav-cta">
          {session ? (
            <Link to="/dashboard" className="lp-btn solid sm">Open dashboard</Link>
          ) : (
            <>
              <Link to="/login" className="lp-btn text sm">Sign in</Link>
              <Link to="/login?mode=register" className="lp-btn solid sm">Get started</Link>
            </>
          )}
        </div>
      </div>
    </header>
  );
}
