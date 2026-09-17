import type { HTMLAttributes } from 'react'
import { CopyButton } from './CopyButton'
import styles from './CodeBlock.module.css'

interface CodeBlockProps extends HTMLAttributes<HTMLPreElement> {
  children?: React.ReactNode
}

export function CodeBlock({ children, ...props }: CodeBlockProps) {
  const code = extractText(children)
  const language = extractLanguage(children)

  return (
    <div className={styles.wrapper}>
      <div className={styles.header}>
        {language && <span className={styles.lang}>{language}</span>}
        <CopyButton value={code} className={styles.copy} />
      </div>
      <pre className={styles.pre} {...props}>
        {children}
      </pre>
    </div>
  )
}

function extractText(node: React.ReactNode): string {
  if (typeof node === 'string') return node
  if (Array.isArray(node)) return node.map(extractText).join('')
  if (node && typeof node === 'object' && 'props' in node) {
    return extractText((node as React.ReactElement).props.children)
  }
  return ''
}

function extractLanguage(children: React.ReactNode): string | null {
  if (children && typeof children === 'object' && 'props' in children) {
    const el = children as React.ReactElement<{ className?: string }>
    const cls = el.props.className ?? ''
    const match = /language-(\w+)/.exec(cls)
    if (match) return match[1]
  }
  return null
}
