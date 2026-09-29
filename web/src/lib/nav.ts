// Lets non-component code (tab actions, palette commands) navigate within
// the router's base path.
let navigateFn: (to: string) => void = (to) => {
  window.location.assign(to.replace(/^\//, ""));
};

export function setNavigator(fn: (to: string) => void) {
  navigateFn = fn;
}

export function go(to: string) {
  navigateFn(to);
}
