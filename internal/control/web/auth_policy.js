// What a refused POST /accounts/{name}/auth/start means.
//
// The authorization step used to catch every failure of that call and render
// the terminal fallback — "run `cloudfs config auth <name>`" — as if the
// backend had never had a browser flow. Only two of the daemon's answers say
// that, and the rest are facts the person needs in order to get anywhere:
//
//   400 err.auth_terminal_only  the credential cannot travel through a
//                               browser (a password, a cookie, a token issued
//                               somewhere else), so there is nothing to drive
//   501 err.auth_unwired        this build has no browser flow for the type
//   409 err.auth_in_progress    another authorization is already running
//   409 err.auth_port_busy      something else holds the loopback callback port
//   502 err.auth_start_failed   the provider refused to begin one
//
// The 409s are the one-at-a-time rule the whole wizard is built around: the
// OAuth callback binds one fixed loopback port, so the second flow cannot
// have it. Told "open a terminal" instead, a person never learns that the
// answer is to finish — or cancel — the flow already in the air.
//
// Nothing here touches the document or the network, so the rule is a fact a
// test can hold rather than a branch buried in a rendering function.
export function authFailureMode(status) {
  return status === 400 || status === 501 ? 'terminal' : 'surface';
}
