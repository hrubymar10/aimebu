import { Component } from '../ui.js';

export class ErrorBoundary extends Component {
  componentDidCatch(error) {
    this.setState({ failed: true });
    this.props.onError?.(error);
  }

  render() { return this.state.failed ? null : this.props.children; }
}
