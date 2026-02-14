import { useState, useEffect } from 'react';
import { authAPI } from '../services/api';
import Navbar from '../components/Navbar';
import './Profile.css';

function Profile() {
  const [user, setUser] = useState(null);
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [showPasswords, setShowPasswords] = useState(false);
  const [isLoading, setIsLoading] = useState(false);
  const [message, setMessage] = useState({ type: '', text: '' });

  useEffect(() => {
    authAPI.me().then(setUser).catch(() => setUser(null));
  }, []);

  const handleSubmit = async (e) => {
    e.preventDefault();
    setMessage({ type: '', text: '' });

    if (newPassword.length < 6) {
      setMessage({ type: 'error', text: 'New password must be at least 6 characters' });
      return;
    }
    if (newPassword !== confirmPassword) {
      setMessage({ type: 'error', text: 'New password and confirmation do not match' });
      return;
    }

    setIsLoading(true);
    try {
      await authAPI.changePassword(currentPassword, newPassword);
      setMessage({ type: 'success', text: 'Password updated successfully.' });
      setCurrentPassword('');
      setNewPassword('');
      setConfirmPassword('');
    } catch (err) {
      setMessage({
        type: 'error',
        text: err.response?.data?.message || 'Failed to update password',
      });
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <div className="profile-page">
      <Navbar title="Profile" />
      <main className="profile-main">
        <div className="profile-card">
          <div className="profile-header">
            <div className="profile-icon">
              <i className="bi bi-person-circle"></i>
            </div>
            <h1>Profile</h1>
            <p className="profile-username">
              <i className="bi bi-person me-2"></i>
              {user ? user.username : '…'}
            </p>
          </div>

          <section className="profile-section">
            <h2>Change password</h2>
            {message.text && (
              <div
                className={`alert ${message.type === 'success' ? 'alert-success' : 'alert-danger'} d-flex align-items-center mb-3`}
                role="alert"
              >
                <i className={`bi ${message.type === 'success' ? 'bi-check-circle-fill' : 'bi-exclamation-triangle-fill'} me-2`}></i>
                {message.text}
              </div>
            )}
            <form onSubmit={handleSubmit}>
              <div className="mb-3">
                <label htmlFor="current_password" className="form-label">
                  <i className="bi bi-lock me-2"></i>Current password
                </label>
                <div className="input-group">
                  <input
                    type={showPasswords ? 'text' : 'password'}
                    id="current_password"
                    className="form-control"
                    value={currentPassword}
                    onChange={(e) => setCurrentPassword(e.target.value)}
                    placeholder="Enter current password"
                    required
                    disabled={isLoading}
                  />
                  <button
                    type="button"
                    className="btn btn-outline-secondary"
                    onClick={() => setShowPasswords(!showPasswords)}
                    aria-label={showPasswords ? 'Hide passwords' : 'Show passwords'}
                  >
                    <i className={`bi ${showPasswords ? 'bi-eye-slash' : 'bi-eye'}`}></i>
                  </button>
                </div>
              </div>
              <div className="mb-3">
                <label htmlFor="new_password" className="form-label">
                  <i className="bi bi-key me-2"></i>New password
                </label>
                <input
                  type={showPasswords ? 'text' : 'password'}
                  id="new_password"
                  className="form-control"
                  value={newPassword}
                  onChange={(e) => setNewPassword(e.target.value)}
                  placeholder="At least 6 characters"
                  required
                  minLength={6}
                  disabled={isLoading}
                />
              </div>
              <div className="mb-4">
                <label htmlFor="confirm_password" className="form-label">
                  <i className="bi bi-key-fill me-2"></i>Confirm new password
                </label>
                <input
                  type={showPasswords ? 'text' : 'password'}
                  id="confirm_password"
                  className="form-control"
                  value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)}
                  placeholder="Confirm new password"
                  required
                  minLength={6}
                  disabled={isLoading}
                />
              </div>
              <button type="submit" className="btn btn-primary btn-save" disabled={isLoading}>
                {isLoading ? (
                  <span className="spinner-border spinner-border-sm me-2"></span>
                ) : (
                  <i className="bi bi-check-lg me-2"></i>
                )}
                Update password
              </button>
            </form>
          </section>
        </div>
      </main>
    </div>
  );
}

export default Profile;
