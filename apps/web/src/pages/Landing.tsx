import { useRef, type MouseEvent, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../state/auth";
import { Logo } from "../components/Logo";
import { useParallax } from "../hooks/useParallax";
import "./landing.css";

const icons: Record<string, ReactNode> = {
  durable: (
    <>
      <ellipse cx="12" cy="6" rx="7" ry="3" />
      <path d="M5 6v6c0 1.7 3.1 3 7 3s7-1.3 7-3V6" />
      <path d="M5 12v6c0 1.7 3.1 3 7 3s7-1.3 7-3v-6" />
    </>
  ),
  editor: (
    <>
      <circle cx="5" cy="6" r="2" />
      <circle cx="19" cy="6" r="2" />
      <circle cx="12" cy="18" r="2" />
      <path d="M7 6h10M6.3 7.7l4.6 8.6M17.7 7.7l-4.6 8.6" />
    </>
  ),
  live: <path d="M3 12h4l3-7 4 14 3-7h4" />,
  replay: (
    <>
      <path d="M4 12a8 8 0 1 0 2.6-5.9" />
      <path d="M4 4v4.5h4.5" />
    </>
  ),
  shield: (
    <>
      <path d="M12 3l8 3v6c0 4.5-3.2 8-8 9-4.8-1-8-4.5-8-9V6z" />
      <path d="M9 12l2 2 4-4" />
    </>
  ),
  observe: <path d="M4 20V11M10 20V4M16 20v-7M22 20V8" />,
};

function Icon({ name }: { name: keyof typeof icons }) {
  return (
    <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {icons[name]}
    </svg>
  );
}

const features: { icon: keyof typeof icons; title: string; body: string }[] = [
  { icon: "durable", title: "Durable by default", body: "Once the API says accepted, the work is in Postgres. Restart any process and the run picks up where it stopped." },
  { icon: "editor", title: "A visual editor that checks your work", body: "Drag nodes onto a canvas and wire them up. Graph and expression errors show as you type, not at 3 a.m." },
  { icon: "live", title: "Watch it run, live", body: "Every input, output, retry and error streams to the browser. Reconnect and it replays what you missed." },
  { icon: "replay", title: "Replay from any node", body: "Re-run a whole execution or restart from the node that failed. History is never rewritten." },
  { icon: "shield", title: "Safe by design", body: "Argon2id passwords, HMAC-signed webhooks, an SSRF-guarded HTTP node and AES-256-GCM secrets." },
  { icon: "observe", title: "Observable", body: "Prometheus metrics, structured JSON logs and OpenTelemetry traces that follow a run from API to worker." },
];

const steps = [
  { n: "01", title: "Draw", body: "Compose triggers, HTTP calls, conditions, loops and delays on a canvas. Validation runs on every change." },
  { n: "02", title: "Publish", body: "Each publish is an immutable version. Runs are pinned to the version they started with, so edits never disturb work in flight." },
  { n: "03", title: "Run", body: "Workers claim tasks with leases, heartbeat while they work, and record every result in a single transaction." },
];

const timeline = [
  { t: "Claim", d: "A worker claims a task with SKIP LOCKED and receives a lease token." },
  { t: "Crash", d: "The worker is killed mid-task. Its heartbeats stop." },
  { t: "Expire", d: "The lease runs out and the scheduler puts the task back in the queue." },
  { t: "Retry", d: "Another worker claims it with a fresh token and does the work." },
  { t: "Complete", d: "The result, next tasks and events commit in one transaction." },
  { t: "Fence", d: "If the first worker wakes up late, its stale token is rejected." },
];

const stats = [
  { v: "443", u: "tasks / second", d: "148 executions per second on a 4-core laptop." },
  { v: "44 ms", u: "p99 latency", d: "End to end at 100 executions per second." },
  { v: "0", u: "failed executions", d: "Across 6,000 runs pushed to twice capacity." },
  { v: "79.9%", u: "statement coverage", d: "205 Go tests, all passing under the race detector." },
];

const built = ["Go", "PostgreSQL", "Redis", "React", "OpenTelemetry", "Prometheus", "Docker"];

function jump(id: string) {
  return (e: MouseEvent) => {
    const t = document.getElementById(id);
    if (!t) return;
    e.preventDefault();
    t.scrollIntoView({ behavior: window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
  };
}

function HeroGraph() {
  const node = (x: number, y: number, title: string, sub: string, state: "ok" | "run" | "idle", accent = false) => (
    <g transform={`translate(${x} ${y})`} className={`g-node ${state}`}>
      <rect width="140" height="52" rx="12" className="g-box" />
      <rect x="0" y="0" width="5" height="52" rx="2.5" className={accent ? "g-bar accent" : "g-bar"} />
      <text x="18" y="22" className="g-title">{title}</text>
      <text x="18" y="39" className="g-sub">{sub}</text>
      <circle cx="124" cy="16" r="4" className="g-dot" />
      <circle cx="124" cy="16" r="4" className="g-ring" />
    </g>
  );
  return (
    <svg viewBox="0 0 726 340" className="lp-graph" role="img" aria-label="Illustration of a workflow: webhook, condition, HTTP request, log and email nodes connected by edges">
      <defs>
        <filter id="lp-shadow" x="-20%" y="-20%" width="140%" height="160%">
          <feDropShadow dx="0" dy="6" stdDeviation="8" floodColor="#5a3218" floodOpacity="0.12" />
        </filter>
      </defs>
      <g className="g-edges">
        <path id="e1" d="M156 172 H196" />
        <path id="e2" d="M336 172 C 368 172, 350 66, 384 66" />
        <path id="e3" d="M336 172 C 368 172, 350 278, 384 278" />
        <path id="e4" d="M524 66 H578" />
      </g>
      <circle r="3.5" className="g-token"><animateMotion dur="3.6s" repeatCount="indefinite" begin="0s"><mpath href="#e1" /></animateMotion></circle>
      <circle r="3.5" className="g-token"><animateMotion dur="3.6s" repeatCount="indefinite" begin="0.9s"><mpath href="#e2" /></animateMotion></circle>
      <circle r="3.5" className="g-token"><animateMotion dur="3.6s" repeatCount="indefinite" begin="1.8s"><mpath href="#e4" /></animateMotion></circle>
      <g filter="url(#lp-shadow)">
        {node(16, 146, "Webhook", "trigger", "ok", true)}
        {node(196, 146, "Condition", "amount > 500", "ok")}
        {node(384, 40, "HTTP request", "POST /approve", "run")}
        {node(384, 252, "Log", "small order", "idle")}
        {node(578, 40, "Email", "notify team", "idle")}
      </g>
      <text x="262" y="128" className="g-label">true</text>
      <text x="262" y="228" className="g-label">false</text>
    </svg>
  );
}

export function Landing() {
  const ref = useRef<HTMLDivElement>(null);
  const { session } = useAuth();
  useParallax(ref);
  const start = session ? "/" : "/login?mode=register";

  return (
    <div className="lp" ref={ref}>
      <div className="lp-progress" aria-hidden="true" />
      <header className="lp-nav">
        <div className="lp-wrap lp-nav-in">
          <Link to="/welcome" className="lp-brand">
            <Logo />
            Synapse
          </Link>
          <nav className="lp-links" aria-label="Sections">
            <a href="#features" onClick={jump("features")}>Features</a>
            <a href="#how" onClick={jump("how")}>How it works</a>
            <a href="#reliability" onClick={jump("reliability")}>Reliability</a>
            <a href="#numbers" onClick={jump("numbers")}>Numbers</a>
          </nav>
          <div className="lp-nav-cta">
            {session ? (
              <Link to="/" className="lp-btn solid sm">Open dashboard</Link>
            ) : (
              <>
                <Link to="/login" className="lp-btn text sm">Sign in</Link>
                <Link to={start} className="lp-btn solid sm">Get started</Link>
              </>
            )}
          </div>
        </div>
      </header>

      <section className="lp-hero">
        <div className="lp-blobs" aria-hidden="true">
          <div className="lp-blob b1" data-speed="0.35" />
          <div className="lp-blob b2" data-speed="0.2" />
          <div className="lp-blob b3" data-speed="0.5" />
        </div>
        <div className="lp-wrap lp-hero-in">
          <div className="lp-hero-copy" data-fade>
            <span className="lp-pill enter" style={{ ["--d" as string]: "0ms" }}>
              <i /> Durable workflow automation
            </span>
            <h1 className="enter" style={{ ["--d" as string]: "80ms" }}>
              Workflows that survive <em>anything</em>.
            </h1>
            <p className="lp-lede enter" style={{ ["--d" as string]: "160ms" }}>
              Draw a graph, publish it, and let a fleet of workers run it. Synapse finishes the job even when a worker, the API or the database restarts halfway through.
            </p>
            <div className="lp-cta enter" style={{ ["--d" as string]: "240ms" }}>
              <Link to={start} className="lp-btn solid lg">{session ? "Open dashboard" : "Get started"}</Link>
              <a href="#how" onClick={jump("how")} className="lp-btn outline lg">See how it works</a>
            </div>
          </div>

          <div className="lp-stage enter" style={{ ["--d" as string]: "320ms" }}>
            <div className="lp-stage-layer" data-speed="-0.06">
              <div className="lp-window">
                <div className="lp-window-bar">
                  <span /><span /><span />
                  <b>order-approval · v3</b>
                  <em>Illustrative editor view</em>
                </div>
                <HeroGraph />
              </div>
            </div>
            <div className="lp-float f1" data-speed="-0.16">
              <div className="lp-chip"><i className="ok" /> Execution succeeded <small>214 ms</small></div>
            </div>
            <div className="lp-float f2" data-speed="-0.28">
              <div className="lp-chip"><i className="warn" /> Retry 2 of 3 <small>backoff 4 s</small></div>
            </div>
            <div className="lp-float f3" data-speed="0.12">
              <div className="lp-chip"><i className="run" /> Lease renewed <small>worker-2</small></div>
            </div>
          </div>
        </div>
      </section>

      <section className="lp-built">
        <div className="lp-wrap reveal">
          <p>Built on</p>
          <ul>
            {built.map((b) => (
              <li key={b}>{b}</li>
            ))}
          </ul>
        </div>
      </section>

      <section id="features" className="lp-section">
        <div className="lp-wrap">
          <div className="lp-head reveal">
            <span className="lp-eyebrow">Features</span>
            <h2>Everything a workflow needs to be trusted</h2>
            <p>A small set of ideas, done properly: state lives in one database, work runs outside the API, and nothing is a black box.</p>
          </div>
          <div className="lp-grid">
            {features.map((f, i) => (
              <article key={f.title} className="lp-card reveal" style={{ ["--i" as string]: i % 3 }}>
                <span className="lp-icon"><Icon name={f.icon} /></span>
                <h3>{f.title}</h3>
                <p>{f.body}</p>
              </article>
            ))}
          </div>
        </div>
      </section>

      <section id="how" className="lp-section lp-how">
        <div className="lp-wrap">
          <div className="lp-head reveal">
            <span className="lp-eyebrow">How it works</span>
            <h2>From canvas to finished run in three steps</h2>
          </div>
          <ol className="lp-steps">
            {steps.map((s, i) => (
              <li key={s.n} className="lp-step reveal" style={{ ["--i" as string]: i }}>
                <div className="lp-step-n"><span data-speed={0.02 + i * 0.02}>{s.n}</span></div>
                <h3>{s.title}</h3>
                <p>{s.body}</p>
              </li>
            ))}
          </ol>
        </div>
      </section>

      <section id="reliability" className="lp-section lp-rel">
        <div className="lp-wrap">
          <div className="lp-head reveal">
            <span className="lp-eyebrow">Reliability</span>
            <h2>A worker dies. The work still finishes.</h2>
            <p>Delivery is at-least-once and completion is accepted once. Leases recover the task, and fencing tokens make sure a slow worker cannot overwrite a newer result.</p>
          </div>
          <ol className="lp-timeline">
            {timeline.map((t, i) => (
              <li key={t.t} className="reveal" style={{ ["--i" as string]: i }}>
                <span className="lp-dot">{i + 1}</span>
                <h3>{t.t}</h3>
                <p>{t.d}</p>
              </li>
            ))}
          </ol>
        </div>
      </section>

      <section id="numbers" className="lp-section">
        <div className="lp-wrap">
          <div className="lp-head reveal">
            <span className="lp-eyebrow">Numbers</span>
            <h2>Measured, not promised</h2>
          </div>
          <div className="lp-stats">
            {stats.map((s, i) => (
              <div key={s.u} className="lp-stat reveal" style={{ ["--i" as string]: i }}>
                <strong>{s.v}</strong>
                <span>{s.u}</span>
                <p>{s.d}</p>
              </div>
            ))}
          </div>
          <p className="lp-fine reveal">
            All figures come from one 4-core laptop where the database, workers and load generator share the same cores, so treat them as a lower bound. Throughput used a relaxed-durability Postgres; with fsync on it was 195 tasks/s. Method and raw results are in docs/benchmarks.
          </p>
        </div>
      </section>

      <section className="lp-final">
        <div className="lp-wrap">
          <div className="lp-final-card reveal">
            <div className="lp-final-glow" aria-hidden="true">
              <div data-speed="0.25" />
            </div>
            <h2>Build a workflow that keeps its promises</h2>
            <p>Create a workspace in under a minute and draw your first graph.</p>
            <div className="lp-cta center">
              <Link to={start} className="lp-btn light lg">{session ? "Open dashboard" : "Get started"}</Link>
              {!session && <Link to="/login" className="lp-btn ghostlight lg">Sign in</Link>}
            </div>
          </div>
        </div>
      </section>

      <footer className="lp-foot">
        <div className="lp-wrap lp-foot-in">
          <Link to="/welcome" className="lp-brand">
            <Logo />
            Synapse
          </Link>
          <p>A durable workflow engine built with Go, PostgreSQL and React.</p>
          <div className="lp-foot-links">
            {session ? (
              <Link to="/">Dashboard</Link>
            ) : (
              <>
                <Link to="/login">Sign in</Link>
                <Link to="/login?mode=register">Register</Link>
              </>
            )}
          </div>
        </div>
      </footer>
    </div>
  );
}
