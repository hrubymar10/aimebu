import { Component } from '../ui.js';
import { captureError } from '../telemetry.js';

export class ErrorBoundary extends Component {
  componentDidCatch(error) {
    this.setState({ failed: true });
    captureError(error, 'render'); // the page it wraps is gone from the screen: this is the only place that knows
    this.props.onError?.(error);
  }

  render() { return this.state.failed ? null : this.props.children; }
}
