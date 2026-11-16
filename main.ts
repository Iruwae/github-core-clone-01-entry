import { createApp } from './app.ts';
// Resolve the coordinator before opening any listening socket.
const coordinatorOrigin = (process.env.MAKEFUN_ENV_URL || '').replace(/\/+$/, '');
if (!coordinatorOrigin) {
 process.stderr.write('[entry] no env to register with: MAKEFUN_ENV_URL is missing\n');
 process.exit(1);
 }
// Each unsuccessful exchange advances a bounded exponential pause.
async function pauseBeforeRetry(failureIndex: number): Promise<void> {
 const pauseLength = Math.min(2000, 250 * (2 ** Math.min(8, failureIndex)));
 await new Promise<void>((finishPause) => { setTimeout(finishPause, pauseLength); });
 }
// Both lifecycle endpoints receive their own thirty-second cancellation signal.
async function exchangeWithCoordinator(resourcePath: string, requestSettings: RequestInit = {}): Promise<Response> {
 return await fetch(coordinatorOrigin + resourcePath, { ...requestSettings, signal: AbortSignal.timeout(30000) });
 }
// Keep polling through transport failures, invalid JSON, and unfinished runs.
async function observeCompletion(): Promise<void> {
 let pollFailures = 0;
 while (true) {
  try {
   const pollReply = await exchangeWithCoordinator('/done');
   if (pollReply.ok) {
    const completionValue: unknown = await pollReply.json();
    if (typeof completionValue === 'object' && completionValue !== null && 'done' in completionValue && completionValue.done === true) break;
   }
  } catch {
   // A failed read supplies no evidence that the coordinator has finished.
	  }
  await pauseBeforeRetry(pollFailures++);
 }
 }
// Registration conflicts are accepted; malformed registration requests are fatal.
async function announceListener(listeningPort: number): Promise<void> {
 let registrationFailures = 0;
 while (true) {
  let registrationReply: Response | undefined;
  try {
   registrationReply = await exchangeWithCoordinator('/register', {
    body: JSON.stringify({ port: listeningPort }),
    headers: { 'content-type': 'application/json' },
    method: 'POST'
   });
  } catch {
   // The coordinator may still be booting, so transport failures are retriable.
	  }
  if (registrationReply) {
   const replyCode = registrationReply.status;
   if (replyCode === 200 || replyCode === 409) break;
   if (replyCode === 400 || replyCode === 405) {
    const rejectionDetails = await registrationReply.text();
    throw new Error(`Registration rejected: HTTP ${replyCode}: ${rejectionDetails}`);
   }
	  }
  await pauseBeforeRetry(registrationFailures++);
 }
 }
// Let the operating system allocate the port, then follow the coordinator's lifecycle.
async function runEntryLifecycle(): Promise<void> {
 const applicationListener = createApp();
 await new Promise<void>((listenerReady, listenerFailed) => {
  applicationListener.once('error', listenerFailed);
  applicationListener.listen(0, listenerReady);
 });
 const boundEndpoint = applicationListener.address();
 if (!boundEndpoint || typeof boundEndpoint === 'string') {
  throw new Error('Expected a TCP address after opening the application listener');
 }
 process.stdout.write(`[entry] listening on port ${boundEndpoint.port}\n`);
 await announceListener(boundEndpoint.port);
 process.stdout.write('[entry] registered; waiting for the env to say done\n');
 await observeCompletion();
 applicationListener.close();
 process.stdout.write('[entry] done\n');
 }
try { await runEntryLifecycle(); process.exit(0); }
catch (lifecycleFailure) {
 process.stderr.write(`[entry] ${lifecycleFailure instanceof Error ? lifecycleFailure.message : String(lifecycleFailure)}\n`);
 process.exit(1);
 }
