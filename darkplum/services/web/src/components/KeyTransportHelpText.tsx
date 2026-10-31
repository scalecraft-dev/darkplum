import { KEY_TRANSPORT_HELP_PARTS, type KeyTransportHelp } from '../lib/vendorKeys'

/** The transport-refusal copy with its commands as inline code. The strings
 *  carry no markdown backticks, because nothing here renders markdown. */
export function KeyTransportHelpText({ kind }: { kind: KeyTransportHelp }) {
  return (
    <>
      {KEY_TRANSPORT_HELP_PARTS[kind].map((part, i) =>
        typeof part === 'string' ? part : <code key={i}>{part.code}</code>,
      )}
    </>
  )
}
