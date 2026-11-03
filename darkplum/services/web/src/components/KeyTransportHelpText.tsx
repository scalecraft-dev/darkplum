import { Fragment } from 'react'
import { CodeBlock } from './ui/CodeBlock'
import {
  KEY_TRANSPORT_HELP_PARTS,
  SECURE_COOKIES_FIX_COMMAND,
  WEB_PORT_FORWARD_COMMAND,
  transportCommandRoles,
  type KeyTransportHelp,
} from '../lib/vendorKeys'

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

/** The commands the copy above points at, in the order it names them: for an
 *  api that is not configured as HTTPS-served, the fix first; then the
 *  port-forward; then the one that installs the key from a terminal. */
export function KeyTransportCommands({
  kind,
  keyCommand,
  withForward = true,
  wrapperClassName,
}: {
  kind: KeyTransportHelp
  keyCommand: string
  withForward?: boolean
  wrapperClassName?: string
}) {
  const commands = { fix: SECURE_COOKIES_FIX_COMMAND, forward: WEB_PORT_FORWARD_COMMAND, key: keyCommand }
  return (
    <>
      {transportCommandRoles(kind, withForward).map((role) => {
        const block = (
          <CodeBlock>
            <code>{commands[role]}</code>
          </CodeBlock>
        )
        return wrapperClassName ? (
          <div key={role} className={wrapperClassName}>
            {block}
          </div>
        ) : (
          <Fragment key={role}>{block}</Fragment>
        )
      })}
    </>
  )
}
